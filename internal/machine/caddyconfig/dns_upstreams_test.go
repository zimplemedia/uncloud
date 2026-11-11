package caddyconfig

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/psviderski/uncloud/internal/machine/store"
	"github.com/psviderski/uncloud/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// acceptingValidator accepts every Caddyfile.
type acceptingValidator struct{}

func (acceptingValidator) Validate(context.Context, string) error { return nil }

func newDNSTestGenerator(dnsUpstreams bool) *CaddyfileGenerator {
	g := NewCaddyfileGenerator("test-machine-id", "test-machine", acceptingValidator{}, nil)
	g.dnsUpstreams = dnsUpstreams
	return g
}

func TestDNSUpstreamsEnabled(t *testing.T) {
	t.Parallel()

	assert.True(t, dnsUpstreamsEnabled("1"))
	for _, v := range []string{"", "0", "true", "yes", "on", " 1", "1 ", "01", "2"} {
		assert.False(t, dnsUpstreamsEnabled(v), "value %q", v)
	}
}

func TestDynamicUpstreamsTemplateFn(t *testing.T) {
	t.Parallel()

	tmplCtx := templateContext{
		Name: "web",
		Upstreams: map[string][]string{
			"web":   {"10.210.0.2", "10.210.1.2"},
			"api":   {"10.210.0.3"},
			"empty": {},
		},
	}
	fn := dynamicUpstreamsTemplateFn(tmplCtx)
	upstreamsFn := upstreamsTemplateFn(tmplCtx)

	block := func(service string, port int) string {
		return "dynamic a " + service + ".internal. " + strconv.Itoa(port) + " {\n" +
			indentMarker + "\trefresh 1s\n" + indentMarker + "\tversions ipv4\n" + indentMarker + "}"
	}

	tests := []struct {
		name    string
		args    []any
		want    string
		wantErr string
	}{
		{name: "no args: current service, default port 80", args: nil, want: block("web", 80)},
		{name: "port for current service", args: []any{8080}, want: block("web", 8080)},
		{name: "service name, default port 80", args: []any{"api"}, want: block("api", 80)},
		{name: "service name and port", args: []any{"api", 3000}, want: block("api", 3000)},
		{name: "zero port means default port", args: []any{"api", 0}, want: block("api", 80)},
		{name: "unknown service renders nothing", args: []any{"unknown", 80}, want: ""},
		{name: "service without healthy containers renders nothing", args: []any{"empty"}, want: ""},
		{name: "invalid type", args: []any{1.5}, wantErr: "dynamic_upstreams function: invalid argument type: float64"},
		{name: "first arg not a string", args: []any{1, 2},
			wantErr: "dynamic_upstreams function: first argument must be service name (string)"},
		{name: "second arg not an int", args: []any{"api", "80"},
			wantErr: "dynamic_upstreams function: second argument must be port (int)"},
		{name: "too many args", args: []any{"api", 80, 1},
			wantErr: "dynamic_upstreams function: too many arguments; expected 0-2, got 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fn(tt.args...)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				// upstreams fails on the same arguments with its own name in the error.
				_, err = upstreamsFn(tt.args...)
				require.EqualError(t, err, strings.Replace(tt.wantErr, "dynamic_upstreams", "upstreams", 1))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)

			// dynamic_upstreams renders nothing exactly when upstreams renders nothing.
			ups, err := upstreamsFn(tt.args...)
			require.NoError(t, err)
			assert.Equal(t, ups == "", got == "", "upstreams=%q", ups)
		})
	}
}

func TestRenderCaddyfileDynamicUpstreamsIndentation(t *testing.T) {
	t.Parallel()

	tmplCtx := templateContext{
		Name:      "web",
		Upstreams: map[string][]string{"web": {"10.210.0.2"}, "api": {"10.210.0.3"}},
	}

	tests := []struct {
		name   string
		config string
		want   string
	}{
		{
			name: "inside reverse_proxy in a site block (tabs)",
			config: `web.example.com {
	reverse_proxy {
		{{dynamic_upstreams 80}}
		import common_proxy
	}
}`,
			want: `web.example.com {
	reverse_proxy {
		dynamic a web.internal. 80 {
			refresh 1s
			versions ipv4
		}
		import common_proxy
	}
}`,
		},
		{
			name: "deeper nesting with spaces and a named service",
			config: `web.example.com {
    handle /api/* {
        reverse_proxy {
            {{dynamic_upstreams "api" 3000}}
        }
    }
}`,
			want: `web.example.com {
    handle /api/* {
        reverse_proxy {
            dynamic a api.internal. 3000 {
            	refresh 1s
            	versions ipv4
            }
        }
    }
}`,
		},
		{
			name: "two calls and upstreams unchanged next to them",
			config: `a.example.com {
	reverse_proxy {
		{{dynamic_upstreams}}
	}
}
b.example.com {
	handle {
		reverse_proxy {
			{{dynamic_upstreams "api" 8080}}
		}
	}
	reverse_proxy {{upstreams "api" 8080}}
}`,
			want: `a.example.com {
	reverse_proxy {
		dynamic a web.internal. 80 {
			refresh 1s
			versions ipv4
		}
	}
}
b.example.com {
	handle {
		reverse_proxy {
			dynamic a api.internal. 8080 {
				refresh 1s
				versions ipv4
			}
		}
	}
	reverse_proxy 10.210.0.3:8080
}`,
		},
		{
			name: "unknown service renders an empty line",
			config: `web.example.com {
	reverse_proxy {
		{{dynamic_upstreams "unknown" 80}}
	}
}`,
			want: "web.example.com {\n\treverse_proxy {\n\t\t\n\t}\n}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderCaddyfile(tmplCtx, tt.config)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, "\x00")
		})
	}
}

// dnsTestRecords returns healthy containers for a mix of generated sites: a service with two replicas on two
// machines publishing HTTP and HTTPS ports, and a hostname shared by two services.
func dnsTestRecords() []store.ContainerRecord {
	return []store.ContainerRecord{
		newContainerRecordWithPorts("api", "10.210.1.3",
			[]string{"api.example.com:8080/http", "api.example.com:8443/https"}, "m2"),
		newContainerRecordWithPorts("api", "10.210.0.3",
			[]string{"api.example.com:8080/http", "api.example.com:8443/https"}, "test-machine-id"),
		newContainerRecordWithPorts("web", "10.210.0.4",
			[]string{"shared.example.com:3000/http", "web.example.com:3000/https"}, "test-machine-id"),
		newContainerRecordWithPorts("admin", "10.210.2.4", []string{"shared.example.com:4000/http"}, "m3"),
	}
}

func TestCaddyfileGeneratorDNSUpstreamsOffIsUnchanged(t *testing.T) {
	t.Parallel()

	// Golden output of the generator before the DNS upstreams mode existed (0.20.0+fix412.7) for dnsTestRecords.
	want := testCaddyfileHeader + `
# Sites generated from service ports.

http://api.example.com {
	reverse_proxy 10.210.0.3:8080 10.210.1.3:8080 {
		import common_proxy
	}
	log
}

http://shared.example.com {
	reverse_proxy 10.210.0.4:3000 10.210.2.4:4000 {
		import common_proxy
	}
	log
}

https://api.example.com {
	reverse_proxy 10.210.0.3:8443 10.210.1.3:8443 {
		import common_proxy
	}
	log
}

https://web.example.com {
	reverse_proxy 10.210.0.4:3000 {
		import common_proxy
	}
	log
}
`
	got, err := newDNSTestGenerator(false).Generate(context.Background(), dnsTestRecords(), true)
	require.NoError(t, err)
	assert.Equal(t, want, normaliseGeneratedTimestamp(got))
}

func TestCaddyfileGeneratorDNSUpstreams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		containers []store.ContainerRecord
		want       string
	}{
		{
			name:       "empty containers",
			containers: nil,
			want:       testCaddyfileHeader,
		},
		{
			name: "replicas, HTTP + HTTPS and a hostname shared by two services",
			containers: append(dnsTestRecords(),
				// Same service, same hostname, second port: becomes a second source in the multi block.
				newContainerRecordWithPorts("admin", "10.210.2.5",
					[]string{"shared.example.com:4000/http", "shared.example.com:4001/http"}, "m3"),
				// Ignored like in the IP mode.
				newContainerRecord(newContainer("", "nonet.example.com:80/http"), "mach1"),
				newContainerRecord(newContainer("10.210.0.9", "tcp.example.com:5432/tcp"), "mach1"),
			),
			want: testCaddyfileHeader + `
# Sites generated from service ports.

http://api.example.com {
	reverse_proxy {
		dynamic a api.internal. 8080 {
			refresh 1s
			versions ipv4
		}
		import common_proxy
	}
	log
}

http://shared.example.com {
	reverse_proxy {
		dynamic multi {
			a admin.internal. 4000 {
				refresh 1s
				versions ipv4
			}
			a admin.internal. 4001 {
				refresh 1s
				versions ipv4
			}
			a web.internal. 3000 {
				refresh 1s
				versions ipv4
			}
		}
		import common_proxy
	}
	log
}

https://api.example.com {
	reverse_proxy {
		dynamic a api.internal. 8443 {
			refresh 1s
			versions ipv4
		}
		import common_proxy
	}
	log
}

https://web.example.com {
	reverse_proxy {
		dynamic a web.internal. 3000 {
			refresh 1s
			versions ipv4
		}
		import common_proxy
	}
	log
}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newDNSTestGenerator(true).Generate(context.Background(), tt.containers, true)
			require.NoError(t, err)
			assert.Equal(t, tt.want, normaliseGeneratedTimestamp(got))

			// The same sites when Caddy is unavailable (custom configs skipped).
			got, err = newDNSTestGenerator(true).Generate(context.Background(), tt.containers, false)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(normaliseGeneratedTimestamp(got), tt.want))
		})
	}
}

func TestHTTPDNSTargetsFromPortsIndependentOfOrder(t *testing.T) {
	t.Parallel()

	records := dnsTestRecords()
	containersOf := func(recs []store.ContainerRecord) []api.ServiceContainer {
		ctrs := make([]api.ServiceContainer, len(recs))
		for i, r := range recs {
			ctrs[i] = r.Container
		}
		return ctrs
	}
	httpA, httpsA := httpDNSTargetsFromPorts(containersOf(records))

	reversed := make([]store.ContainerRecord, len(records))
	for i, r := range records {
		reversed[len(records)-1-i] = r
	}
	// Extra replicas of the same services must not add or reorder targets.
	reversed = append(reversed,
		newContainerRecordWithPorts("web", "10.210.3.4",
			[]string{"shared.example.com:3000/http", "web.example.com:3000/https"}, "m4"),
		newContainerRecordWithPorts("api", "10.210.3.3",
			[]string{"api.example.com:8080/http", "api.example.com:8443/https"}, "m4"),
	)
	httpB, httpsB := httpDNSTargetsFromPorts(containersOf(reversed))

	assert.Equal(t, httpA, httpB)
	assert.Equal(t, httpsA, httpsB)
	assert.Equal(t, []dnsTarget{{"admin", 4000}, {"web", 3000}}, httpA["shared.example.com"])
}

// TestDNSUpstreamsReplacementKeepsCaddyfileBody is the key property of the DNS upstreams: replacing a service
// container (old healthy -> old + new healthy -> new only, different IPs) doesn't change the Caddyfile body, so the
// controller skips the Caddy load. The first healthy container of a service appearing or the last one going away
// still changes it.
func TestDNSUpstreamsReplacementKeepsCaddyfileBody(t *testing.T) {
	t.Parallel()

	const xcaddy = `site.example.com {
	reverse_proxy {
		{{dynamic_upstreams 80}}
		import common_proxy
	}
}
other.example.com {
	reverse_proxy {
		{{dynamic_upstreams "app" 8080}}
	}
}`
	created := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	// Other services that stay unchanged during the rollouts.
	stable := []store.ContainerRecord{
		newContainerRecordWithCaddyConfig("caddy", "10.210.0.1", "{\n\tdebug\n}", "test-machine-id", created),
		newContainerRecordWithPorts("db-admin", "10.210.0.20", []string{"dbadmin.example.com:8081/https"}, "m2"),
	}
	site := func(ip, machine string, at time.Time) store.ContainerRecord {
		return newContainerRecordWithCaddyConfig("site", ip, xcaddy, machine, at)
	}
	app := func(ip, machine string) store.ContainerRecord {
		return newContainerRecordWithPorts("app", ip, []string{"app.example.com:8080/http"}, machine)
	}
	with := func(recs ...store.ContainerRecord) []store.ContainerRecord {
		return append(append([]store.ContainerRecord{}, stable...), recs...)
	}

	body := func(t *testing.T, dnsUpstreams bool, recs []store.ContainerRecord) string {
		// Generate sorts the records in place and the parallel subtests share them, so pass a copy.
		got, err := newDNSTestGenerator(dnsUpstreams).Generate(context.Background(), slices.Clone(recs), true)
		require.NoError(t, err)
		return caddyfileBody(got)
	}

	steady := with(site("10.210.0.5", "test-machine-id", created), app("10.210.0.6", "test-machine-id"))
	steps := map[string][]store.ContainerRecord{
		// Rolling update of both services: the new containers start healthy next to the old ones.
		"old + new healthy": with(
			site("10.210.0.5", "test-machine-id", created), site("10.210.1.15", "m2", created.Add(time.Minute)),
			app("10.210.0.6", "test-machine-id"), app("10.210.1.16", "m2"),
		),
		// The old containers are removed.
		"new only": with(site("10.210.1.15", "m2", created.Add(time.Minute)), app("10.210.1.16", "m2")),
		// Old container unhealthy while stopping, new one healthy.
		"old unhealthy + new healthy": with(
			unhealthy(site("10.210.0.5", "test-machine-id", created)), site("10.210.1.15", "m2", created.Add(time.Minute)),
			unhealthy(app("10.210.0.6", "test-machine-id")), app("10.210.1.16", "m2"),
		),
	}

	t.Run("DNS upstreams on: replacement keeps the body", func(t *testing.T) {
		t.Parallel()
		want := body(t, true, steady)
		require.Contains(t, want, "dynamic a site.internal. 80 {")
		require.Contains(t, want, "dynamic a app.internal. 8080 {")
		require.Contains(t, want, "dynamic a db-admin.internal. 8081 {")
		require.NotContains(t, want, "10.210.0.5")
		require.NotContains(t, want, "10.210.0.6")
		for name, recs := range steps {
			assert.Equal(t, want, body(t, true, recs), name)
		}
	})

	t.Run("DNS upstreams on: first or last healthy container changes the body", func(t *testing.T) {
		t.Parallel()
		want := body(t, true, steady)

		noSite := with(app("10.210.0.6", "test-machine-id"))
		assert.NotEqual(t, want, body(t, true, noSite), "last site container gone")
		assert.NotContains(t, body(t, true, noSite), "site.internal.")

		noApp := with(site("10.210.0.5", "test-machine-id", created))
		got := body(t, true, noApp)
		assert.NotEqual(t, want, got, "last app container gone")
		assert.NotContains(t, got, "app.example.com", "generated site removed")
		assert.NotContains(t, got, "app.internal.", "dynamic_upstreams for app renders nothing")

		allUnhealthy := with(unhealthy(site("10.210.0.5", "test-machine-id", created)),
			unhealthy(app("10.210.0.6", "test-machine-id")))
		assert.Equal(t, body(t, true, with()), body(t, true, allUnhealthy))
		assert.NotEqual(t, want, body(t, true, allUnhealthy), "no healthy containers")
	})

	t.Run("DNS upstreams off: replacement still changes the generated sites", func(t *testing.T) {
		t.Parallel()
		want := body(t, false, steady)
		for name, recs := range steps {
			assert.NotEqual(t, want, body(t, false, recs), name)
		}
	})
}

// unhealthy returns a copy of the record with a stopped container.
func unhealthy(cr store.ContainerRecord) store.ContainerRecord {
	state := *cr.Container.State
	state.Running = false
	cr.Container.State = &state
	return cr
}
