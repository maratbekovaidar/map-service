package httpapi

import (
	"bytes"
	"net/http"

	"go.yaml.in/yaml/v3"

	"map-service/api"
)

// docsFiles are the Swagger UI files of the /docs page (STATIC_DIR/vendor).
var docsFiles = map[string]string{
	"swagger-ui-bundle.js": "text/javascript; charset=utf-8",
	"swagger-ui.css":       "text/css; charset=utf-8",
}

// GET /docs - Swagger UI over /openapi.yaml.
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) error {
	serveBytes(w, r, "text/html; charset=utf-8", "public, max-age=300", api.DocsHTML)
	return nil
}

// GET /docs/{file} - the vendored Swagger UI bundle and stylesheet.
func (s *Server) handleDocsAsset(w http.ResponseWriter, r *http.Request) error {
	return s.serveVendor(w, r, docsFiles)
}

// GET /openapi.yaml - the embedded spec with `servers` pointing at this very
// service, so "Try it out" in /docs calls the host the page was opened on.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) error {
	b, err := specForServer(api.OpenAPI, s.static.publicBase(r))
	if err != nil {
		return internalError(err)
	}
	serveBytes(w, r, "application/yaml", "public, max-age=300", b)
	return nil
}

// specForServer replaces the `servers` list of an OpenAPI document with a
// single entry for baseURL. The document is edited as a YAML node tree so
// comments, block scalars and key order of the rest survive.
func specForServer(spec []byte, baseURL string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, err
	}
	root := doc.Content[0]
	servers := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{{
		Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			scalar("url"), scalar(baseURL),
			scalar("description"), scalar("This server"),
		},
	}}}
	replaced := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "servers" {
			root.Content[i+1] = servers
			replaced = true
			break
		}
	}
	if !replaced {
		root.Content = append(root.Content, scalar("servers"), servers)
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}
