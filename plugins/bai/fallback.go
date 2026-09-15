package main

import (
	"encoding/json"
	"strings"
)

// freeModelOrder is the stable candidate order used for automatic fallback
// between free models when the requested one fails with a retryable error.
// Reliable-zero-balance models (measured 2026-09-14) come first; glm-5.3-flash
// is listed last because it went credit-gated but clients still request it by
// name and must keep auto-switching. deepseek-v4-flash is unverified since it
// dropped out of the reliable-free set.
var freeModelOrder = []string{"qwen3.8-flash", "hy3", "mimo-v2.5", "glm-5.3-flash", "deepseek-v4-flash"}

// freeSwitchable reports whether requests for a native model may be
// transparently rewritten onto another free model. True for models the latest
// probe calls free, and for the static free catalogue: a catalogue member that
// was demoted (e.g. glm-5.3-flash after the insufficient_user_quota change)
// must keep fallback coverage, otherwise its requests hard-fail instead of
// switching.
func freeSwitchable(requested string) bool {
	if probeClassForModel(requested) == probeFree {
		return true
	}
	for _, id := range freeModelOrder {
		if id == requested {
			return true
		}
	}
	return false
}

// attemptPayloads returns the payload chain for one request: the normalized
// original first, then — only when the requested model is switchable (see
// freeSwitchable) — the same payload rewritten to each other free candidate.
// Premium and unknown models are never silently switched.
func attemptPayloads(base []byte) [][]byte {
	attempts := [][]byte{base}
	requested := requestedNativeModel(base)
	if requested == "" || !freeSwitchable(requested) {
		return attempts
	}
	for _, id := range fallbackCandidates(requested) {
		attempts = append(attempts, payloadWithNativeModel(base, id))
	}
	return attempts
}

// requestedNativeModel returns the native B.AI model name after payload
// normalization. Both "bai-x" and the already-rewritten "x (free)" forms are
// accepted; anything else returns "".
func requestedNativeModel(payload []byte) string {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return ""
	}
	m, _ := body["model"].(string)
	if m == "" {
		return ""
	}
	native, found := strings.CutPrefix(m, "bai-")
	if !found {
		native = m
	}
	return strings.TrimSuffix(native, " (free)")
}

// payloadWithNativeModel rewrites the payload model field to a native id.
func payloadWithNativeModel(payload []byte, native string) []byte {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return payload
	}
	if _, ok := body["model"].(string); !ok {
		return payload
	}
	body["model"] = native
	out, err := json.Marshal(body)
	if err != nil {
		return payload
	}
	return out
}

// fallbackCandidates lists other free models to try: models whose latest
// reliable probe says free come first, remaining known-free ids after.
func fallbackCandidates(requested string) []string {
	reliable := make([]string, 0, len(freeModelOrder))
	rest := make([]string, 0, len(freeModelOrder))
	for _, id := range freeModelOrder {
		if id == requested {
			continue
		}
		if probeClassForModel(id) == probeFree {
			reliable = append(reliable, id)
		} else {
			rest = append(rest, id)
		}
	}
	return append(reliable, rest...)
}

// fallbackEligible reports whether a failed attempt is worth retrying on
// another free model. Credential problems and malformed requests are not:
// switching models cannot fix those.
func fallbackEligible(class string) bool {
	return class == "rate_limited" || class == "upstream_unavailable" || class == "insufficient_balance"
}
