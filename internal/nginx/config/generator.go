package config

import (
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/gateway-api/apis/v1alpha2"

	"github.com/nginxinc/nginx-kubernetes-gateway/internal/state"
)

// nginx502Server is used as a backend for services that cannot be resolved (have no IP address).
const nginx502Server = "unix:/var/lib/nginx/nginx-502-server.sock"

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 . Generator

// Generator generates NGINX configuration.
type Generator interface {
	// GenerateForHost generates configuration for a host.
	GenerateForHost(host state.Host) ([]byte, Warnings)
}

// GeneratorImpl is an implementation of Generator
type GeneratorImpl struct {
	executor     *templateExecutor
	serviceStore state.ServiceStore
}

// NewGeneratorImpl creates a new GeneratorImpl.
func NewGeneratorImpl(serviceStore state.ServiceStore) *GeneratorImpl {
	return &GeneratorImpl{
		executor:     newTemplateExecutor(),
		serviceStore: serviceStore,
	}
}

func (g *GeneratorImpl) GenerateForHost(host state.Host) ([]byte, Warnings) {
	server, warnings := generate(host, g.serviceStore)
	return g.executor.ExecuteForServer(server), warnings
}

func generate(host state.Host, serviceStore state.ServiceStore) (server, Warnings) {
	warnings := newWarnings()

	locs := make([]location, 0, len(host.PathRouteGroups)) // FIXME(pleshakov): expand with g.Routes

	for _, g := range host.PathRouteGroups {

		matches := make([]httpMatch, 0, len(g.Routes))

		for routeIdx, r := range g.Routes {
			address, err := getBackendAddress(
				r.Source.Spec.Rules[r.RuleIdx].BackendRefs,
				r.Source.Namespace,
				serviceStore,
			)

			if err != nil {
				warnings.AddWarning(r.Source, err.Error())
			}

			path := createPathForMatch(g.Path, routeIdx)

			locs = append(locs, generateMatchLocation(path, address))
			matches = append(matches, createHTTPMatch(r.GetMatch(), path))

		}

		if len(matches) > 0 {
			b, err := json.Marshal(matches)

			if err != nil {
				// panic is safe here because we should never fail to marshal the match unless we constructed it incorrectly.
				panic(fmt.Errorf("could not marshal http match: %w", err))
			}

			pathLoc := location{
				Path:         g.Path,
				HTTPMatchVar: string(b),
			}

			locs = append(locs, pathLoc)
		}
	}

	return server{
		ServerName: host.Value,
		Locations:  locs,
	}, warnings
}

func generateProxyPass(address string) string {
	if address == "" {
		return "http://" + nginx502Server
	}
	return "http://" + address
}

func getBackendAddress(
	refs []v1alpha2.HTTPBackendRef,
	parentNS string,
	serviceStore state.ServiceStore,
) (string, error) {
	if len(refs) == 0 {
		return "", errors.New("empty backend refs")
	}

	// FIXME(pleshakov): for now, we only support a single backend reference
	ref := refs[0].BackendRef

	if ref.Kind != nil && *ref.Kind != "Service" {
		return "", fmt.Errorf("unsupported kind %s", *ref.Kind)
	}

	ns := parentNS
	if ref.Namespace != nil {
		ns = string(*ref.Namespace)
	}

	address, err := serviceStore.Resolve(types.NamespacedName{Namespace: ns, Name: string(ref.Name)})
	if err != nil {
		return "", fmt.Errorf("service %s/%s cannot be resolved: %w", ns, ref.Name, err)
	}

	if ref.Port == nil {
		return "", errors.New("port is nil")
	}

	return fmt.Sprintf("%s:%d", address, *ref.Port), nil
}

func generateMatchLocation(path, address string) location {
	return location{
		Path:      path,
		ProxyPass: generateProxyPass(address),
		Internal:  true,
	}
}

func createPathForMatch(path string, routeIdx int) string {
	return fmt.Sprintf("%s_route%d", path, routeIdx)
}

// httpMatch is an internal representation of an HTTPRouteMatch.
// This struct is marshaled into a string and stored as a variable in the nginx location block for the route's path.
// The NJS httpmatches module will lookup this variable on the request object and compare the request against the Method, Headers, and QueryParams contained in httpMatch.
// If the request satisfies the httpMatch, the request will be internally redirected to the location RedirectPath by NGINX.
type httpMatch struct {
	// Any represents a match with no match conditions.
	Any bool `json:"any,omitempty"`
	// Method is the HTTPMethod of the HTTPRouteMatch.
	Method v1alpha2.HTTPMethod `json:"method,omitempty"`
	// Headers is a list of HTTPHeaders name value pairs with the format "{name}:{value}".
	Headers []string `json:"headers,omitempty"`
	// QueryParams is a list of HTTPQueryParams name value pairs with the format "{name}={value}".
	QueryParams []string `json:"params,omitempty"`
	// RedirectPath is the path to redirect the request to if the request satisfies the match conditions.
	RedirectPath string `json:"redirectPath,omitempty"`
}

func createHTTPMatch(match v1alpha2.HTTPRouteMatch, redirectPath string) httpMatch {
	hm := httpMatch{
		RedirectPath: redirectPath,
	}

	if isPathOnlyMatch(match) {
		hm.Any = true
		return hm
	}

	if match.Method != nil {
		hm.Method = *match.Method
	}

	if match.Headers != nil {
		headers := make([]string, 0, len(match.Headers))

		// FIXME(kate-osborn): For now we only support type "Exact".
		for _, h := range match.Headers {
			if *h.Type == v1alpha2.HeaderMatchExact {
				headers = append(headers, createHeaderKeyValString(h))
			}
		}
		hm.Headers = headers
	}

	if match.QueryParams != nil {
		params := make([]string, 0, len(match.QueryParams))

		// FIXME(kate-osborn): For now we only support type "Exact".
		for _, p := range match.QueryParams {
			if *p.Type == v1alpha2.QueryParamMatchExact {
				params = append(params, createQueryParamKeyValString(p))
			}
		}
		hm.QueryParams = params
	}

	return hm
}

// The name and values are delimited by "=". A name and value can always be recovered using strings.SplitN(arg,"=", 2).
// Query Parameters are case-sensitive so case is preserved.
func createQueryParamKeyValString(p v1alpha2.HTTPQueryParamMatch) string {
	return p.Name + "=" + p.Value
}

// The name and values are delimited by ":". A name and value can always be recovered using strings.Split(arg, ":").
// Header names are case-insensitive while header values are case-sensitive (e.g. foo:bar == FOO:bar, but foo:bar != foo:BAR).
// We preserve the case of the name here because NGINX allows us to lookup the header names in a case-insensitive manner.
func createHeaderKeyValString(h v1alpha2.HTTPHeaderMatch) string {
	return string(h.Name) + ":" + h.Value
}

func isPathOnlyMatch(match v1alpha2.HTTPRouteMatch) bool {
	return match.Method == nil && match.Headers == nil && match.QueryParams == nil
}
