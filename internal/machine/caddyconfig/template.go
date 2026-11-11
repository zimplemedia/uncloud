package caddyconfig

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	// dnsUpstreamsRefresh is how often Caddy re-resolves a dynamic A upstream. The uncloud DNS answers with TTL 0,
	// so this is the effective cache time of a lookup in Caddy.
	dnsUpstreamsRefresh = "1s"
	// defaultUpstreamPort is the port Caddy dials when an upstream address has no port and no scheme. A dynamic A
	// upstream without a port defaults to the same port, so it's rendered explicitly.
	defaultUpstreamPort = 80
	// indentMarker prefixes the continuation lines of a multi-line block rendered by a template function. Template
	// functions can't see where they are called, so renderCaddyfile replaces the marker with the leading whitespace of
	// the line the block starts on. This keeps the block indented correctly at any nesting level.
	indentMarker = "\x00uncloud-indent\x00"
)

// templateContext holds the data available to Caddyfile templates.
type templateContext struct {
	// Name is the current service name.
	Name string
	// Upstreams maps service names to their container IPs.
	Upstreams map[string][]string
}

// parseUpstreamsArgs parses the arguments shared by the upstreams and dynamic_upstreams template functions:
// [service-name] [port]. Port 0 means no port was specified.
func parseUpstreamsArgs(fnName string, tmplCtx templateContext, args []any) (string, int, error) {
	var serviceName string
	var port int

	switch len(args) {
	case 0:
		// Current service, default port.
		serviceName = tmplCtx.Name
	case 1:
		// Either port (int) for current service or service name (string).
		switch arg := args[0].(type) {
		case int:
			serviceName = tmplCtx.Name
			port = arg
		case string:
			serviceName = arg
			port = 0
		default:
			return "", 0, fmt.Errorf("%s function: invalid argument type: %T", fnName, arg)
		}
	case 2:
		// Service name and port.
		name, ok := args[0].(string)
		if !ok {
			return "", 0, fmt.Errorf("%s function: first argument must be service name (string)", fnName)
		}
		serviceName = name

		p, ok := args[1].(int)
		if !ok {
			return "", 0, fmt.Errorf("%s function: second argument must be port (int)", fnName)
		}
		port = p
	default:
		return "", 0, fmt.Errorf("%s function: too many arguments; expected 0-2, got %d", fnName, len(args))
	}

	return serviceName, port, nil
}

// upstreamsTemplateFn returns a template function that generates a space separated string of upstreams for the service.
// It optionally accepts a service name and a port number: {{upstreams [service-name] [port]}}.
func upstreamsTemplateFn(tmplCtx templateContext) func(args ...any) (string, error) {
	return func(args ...any) (string, error) {
		serviceName, port, err := parseUpstreamsArgs("upstreams", tmplCtx, args)
		if err != nil {
			return "", err
		}

		ips, ok := tmplCtx.Upstreams[serviceName]
		if !ok || len(ips) == 0 {
			// No upstreams available.
			return "", nil
		}

		// Build the space separated upstreams string.
		var upstreams []string
		for _, ip := range ips {
			if port > 0 {
				upstreams = append(upstreams, net.JoinHostPort(ip, strconv.Itoa(port)))
			} else {
				upstreams = append(upstreams, ip)
			}
		}

		return strings.Join(upstreams, " "), nil
	}
}

// dynamicUpstreamsTemplateFn returns a template function that generates a 'dynamic a' upstream source for the service
// that resolves its healthy containers through the uncloud internal DNS at request time:
// {{dynamic_upstreams [service-name] [port]}}. It accepts the same arguments as the upstreams function and must be
// used inside a reverse_proxy block:
//
//	reverse_proxy {
//		{{dynamic_upstreams 80}}
//	}
//
// Unlike the upstreams function, the output doesn't change when the service's containers are replaced, so Caddy
// doesn't need to reload. Like the upstreams function, it renders nothing when the service has no healthy containers.
func dynamicUpstreamsTemplateFn(tmplCtx templateContext) func(args ...any) (string, error) {
	return func(args ...any) (string, error) {
		serviceName, port, err := parseUpstreamsArgs("dynamic_upstreams", tmplCtx, args)
		if err != nil {
			return "", err
		}

		if ips, ok := tmplCtx.Upstreams[serviceName]; !ok || len(ips) == 0 {
			// No upstreams available.
			return "", nil
		}
		if port <= 0 {
			port = defaultUpstreamPort
		}

		return dynamicABlock("dynamic a", serviceName, port, indentMarker), nil
	}
}

// dynamicABlock renders a 'dynamic a' upstream source block for the service. The first line is not indented, the
// following lines are prefixed with indent. head is "dynamic a" for a standalone source or "a" for a source inside
// a 'dynamic multi' block.
//
// The name has a trailing dot so that it's not expanded with the search domains of the Caddy container
// (ndots:1, search internal.) and only IPv4 addresses are looked up as the uncloud network is IPv4 only.
func dynamicABlock(head, serviceName string, port int, indent string) string {
	return fmt.Sprintf("%s %s.internal. %d {\n%s\trefresh %s\n%s\tversions ipv4\n%s}",
		head, serviceName, port, indent, dnsUpstreamsRefresh, indent, indent)
}

// expandIndentMarkers replaces each indentMarker at the start of a line with the leading whitespace of the nearest
// preceding line that doesn't start with the marker, i.e. the line the multi-line block starts on.
func expandIndentMarkers(s string) string {
	if !strings.Contains(s, indentMarker) {
		return s
	}

	lines := strings.Split(s, "\n")
	indent := ""
	for i, line := range lines {
		if rest, ok := strings.CutPrefix(line, indentMarker); ok {
			lines[i] = indent + rest
			continue
		}
		indent = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	}
	// A marker can only end up elsewhere if the block was embedded in the middle of a line, e.g. passed to another
	// template function. Drop it rather than leaving a NUL byte in the Caddyfile.
	return strings.ReplaceAll(strings.Join(lines, "\n"), indentMarker, "")
}
