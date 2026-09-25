package gateway

import (
	"gopkg.in/yaml.v3"
)

// File provider rendering (Traefik dynamic configuration, YAML form). This
// is the fully-supported reference target: test/gateway runs a real Traefik
// binary against exactly this output.

type fileConfig struct {
	HTTP fileHTTP `yaml:"http"`
}

type fileHTTP struct {
	Routers     map[string]fileRouter     `yaml:"routers,omitempty"`
	Middlewares map[string]fileMiddleware `yaml:"middlewares,omitempty"`
	Services    map[string]fileService    `yaml:"services,omitempty"`
}

type fileRouter struct {
	Rule        string   `yaml:"rule"`
	Service     string   `yaml:"service,omitempty"`
	Middlewares []string `yaml:"middlewares,omitempty"`
	EntryPoints []string `yaml:"entryPoints,omitempty"`
	ParentRefs  []string `yaml:"parentRefs,omitempty"`
	Priority    int      `yaml:"priority,omitempty"`
}

type fileMiddleware struct {
	Headers     *fileHeaders     `yaml:"headers,omitempty"`
	ForwardAuth *fileForwardAuth `yaml:"forwardAuth,omitempty"`
}

type fileHeaders struct {
	CustomRequestHeaders map[string]string `yaml:"customRequestHeaders,omitempty"`
}

type fileForwardAuth struct {
	Address                  string   `yaml:"address"`
	AuthResponseHeaders      []string `yaml:"authResponseHeaders,omitempty"`
	AuthResponseHeadersRegex string   `yaml:"authResponseHeadersRegex,omitempty"`
}

type fileService struct {
	LoadBalancer fileLoadBalancer `yaml:"loadBalancer"`
}

type fileLoadBalancer struct {
	Servers []fileServer `yaml:"servers"`
}

type fileServer struct {
	URL string `yaml:"url"`
}

// FileYAML renders the configuration for the Traefik file provider.
func (c *Configuration) FileYAML() ([]byte, error) {
	if err := c.ValidateHierarchy(); err != nil {
		return nil, err
	}
	out := fileConfig{}
	out.HTTP.Routers = map[string]fileRouter{}
	out.HTTP.Middlewares = map[string]fileMiddleware{}
	out.HTTP.Services = map[string]fileService{}

	for _, r := range c.Routers {
		fr := fileRouter{
			Rule: r.Rule, Service: r.Service,
			Middlewares: nilIfEmpty(r.Middlewares),
			EntryPoints: nilIfEmpty(r.EntryPoints),
			ParentRefs:  nilIfEmpty(r.ParentRefs),
			Priority:    r.Priority,
		}
		out.HTTP.Routers[r.Name] = fr
	}
	for _, m := range c.Middlewares {
		fm := fileMiddleware{}
		if m.ForwardAuth != nil {
			fm.ForwardAuth = &fileForwardAuth{
				Address:                  m.ForwardAuth.Address,
				AuthResponseHeaders:      nilIfEmpty(m.ForwardAuth.AuthResponseHeaders),
				AuthResponseHeadersRegex: m.ForwardAuth.AuthResponseHeadersRegex,
			}
		} else {
			headers := map[string]string{}
			for _, h := range m.StripHeaders {
				headers[h] = "" // empty value = remove the header
			}
			fm.Headers = &fileHeaders{CustomRequestHeaders: headers}
		}
		out.HTTP.Middlewares[m.Name] = fm
	}
	for _, s := range c.Services {
		servers := make([]fileServer, 0, len(s.Servers))
		for _, u := range s.Servers {
			servers = append(servers, fileServer{URL: u})
		}
		out.HTTP.Services[s.Name] = fileService{
			LoadBalancer: fileLoadBalancer{Servers: servers},
		}
	}
	return yaml.Marshal(out)
}

// FileJSON renders the same configuration as JSON — the file provider
// accepts YAML, TOML and JSON interchangeably.
func (c *Configuration) FileJSON() ([]byte, error) {
	y, err := c.FileYAML()
	if err != nil {
		return nil, err
	}
	var v any
	if err := yaml.Unmarshal(y, &v); err != nil {
		return nil, err
	}
	return jsonMarshal(v)
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
