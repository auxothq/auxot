package openai

import "encoding/json"

// NormalizeVisionContent converts multimodal content from Auxot's flat wire shape
// ({ "type": "image_url", "image_url": "<data or https URL>" }) into the OpenAI
// shape ({ "type": "image_url", "image_url": { "url": "..." } }) required by
// llama.cpp and strict OpenAI-compatible APIs. Parts that already use the object
// form are left unchanged.
func NormalizeVisionContent(content json.RawMessage) json.RawMessage {
	if len(content) == 0 {
		return content
	}
	switch content[0] {
	case '"':
		return content
	case '[':
		// continue
	default:
		return content
	}

	var parts []json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return content
	}

	changed := false
	out := make([]json.RawMessage, len(parts))
	for i, part := range parts {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(part, &obj); err != nil {
			out[i] = part
			continue
		}
		if string(obj["type"]) != `"image_url"` {
			out[i] = part
			continue
		}
		urlRaw, ok := obj["image_url"]
		if !ok || len(urlRaw) == 0 {
			out[i] = part
			continue
		}
		if urlRaw[0] == '{' {
			out[i] = part
			continue
		}
		if urlRaw[0] != '"' {
			out[i] = part
			continue
		}
		var urlStr string
		if err := json.Unmarshal(urlRaw, &urlStr); err != nil || urlStr == "" {
			out[i] = part
			continue
		}
		nested, err := json.Marshal(map[string]string{"url": urlStr})
		if err != nil {
			out[i] = part
			continue
		}
		obj["image_url"] = nested
		newPart, err := json.Marshal(obj)
		if err != nil {
			out[i] = part
			continue
		}
		out[i] = newPart
		changed = true
	}
	if !changed {
		return content
	}
	result, err := json.Marshal(out)
	if err != nil {
		return content
	}
	return result
}
