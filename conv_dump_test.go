package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestDumpConvertedBodies(t *testing.T) {
	cfg := pluginConfig{}
	cfg.Models = append(cfg.Models, modelRoute{Model: "mimo-v2.6-flash-free", Alias: "mimo-v2.6-flash-free", Endpoint: "chat"})
	route, _ := routeForModel(cfg, "mimo-v2.6-flash-free")

	cases := map[string]string{
		"minimal":  `{"model":"mimo-v2.6-flash-free","stream":true,"input":"Hello"}`,
		"pi-shape": `{"model":"mimo-v2.6-flash-free","store":false,"stream":true,"instructions":"You are a helpful assistant.","input":[{"role":"user","content":[{"type":"input_text","text":"Hello"}]}],"text":{"verbosity":"low"},"include":["reasoning.encrypted_content"],"prompt_cache_key":"k1","tool_choice":"auto","parallel_tool_calls":true,"tools":[{"type":"function","name":"bash","strict":null,"description":"Run","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],"reasoning":{"effort":"high","summary":"auto"}}`,
	}
	for name, raw := range cases {
		req := executorRequest{Model: "mimo-v2.6-flash-free", Payload: []byte(raw)}
		out, err := prepareUpstreamBody(req, route)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var pretty map[string]any
		json.Unmarshal(out, &pretty)
		keys := make([]string, 0)
		for k := range pretty {
			keys = append(keys, k)
		}
		fmt.Printf("== %s ==\nkeys: %v\nbody: %s\n\n", name, keys, out)
	}
}
