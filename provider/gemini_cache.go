package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/genai"
)

// Explicit prompt caching for the Gemini (google) path via the genai SDK's
// `CachedContent` resource.
//
// Unlike Anthropic (inline `cache_control` breakpoints decided per request),
// Gemini caches a SEPARATELY-CREATED resource referenced by name with a TTL.
// We cache the most stable Gemini-side prefix — the system instruction plus the
// tool declarations — because that prefix is large, identical across every turn
// of a session, and therefore the highest-ROI thing to cache.
//
// ECONOMICS / TTL (no delete needed):
//   - A CachedContent resource AUTO-EXPIRES at its TTL; there is NO need to call
//     Delete for correctness. Storage billing simply stops at expiry. Delete
//     would only ever be an optional early-storage-reclaim optimization, which
//     we deliberately skip in v1.
//   - We pick a SHORT TTL (GW_GEMINI_CACHE_TTL, default 10m) sized to the gap
//     between agent turns, so we never pay storage for a cache that won't be
//     read back within the session's cadence — the same "don't cache what won't
//     be read" rule applied to the TIME dimension.
//   - We only create a cache when PlanCache says the system prefix is worth it
//     (large + multi-turn) AND it clears the Gemini explicit-cache minimum
//     (geminiCacheMinBytes), which is higher than the inline-marker minimum.
//
// FAILURE-SAFE: any error creating or reusing a cache resource degrades
// gracefully to a normal, uncached request. A cache miss must never break
// generation.

// GW_GEMINI_CACHE_TTL: lifetime of a Gemini CachedContent resource. Default 10
// minutes — long enough to span typical agent turn gaps, short enough to avoid
// paying storage for stale caches. Expressed in seconds.
var geminiCacheTTLSecs = envInt32("GW_GEMINI_CACHE_TTL", 600)

// geminiCacheMinBytes is the minimum system+tools prefix size before we bother
// minting an explicit Gemini cache. Vertex's explicit context-cache minimum is
// substantially larger than the inline-marker minimum (historically ~32k
// tokens for some models; the SDK rejects sub-minimum creates). We use a
// conservative byte heuristic (~128 KB ≈ 32k tokens) so a create attempt that
// would be rejected as too-small is avoided up front.
var geminiCacheMinBytes = envInt32("GW_GEMINI_CACHE_MIN_BYTES", 128_000)

// geminiCacheEntry is one row in the in-memory cache-resource registry: the
// upstream resource name plus our locally-tracked expiry. We treat the entry as
// usable only until shortly BEFORE expiry to avoid a race where the resource
// lapses mid-request.
type geminiCacheEntry struct {
	name   string
	expiry time.Time
}

// geminiCacheRegistry is a tiny TTL-aware map of prefix-hash -> resource name.
// It is process-local (single-tenant scope) and self-pruning on lookup. No
// delete of the upstream resource is performed — entries simply fall out of the
// map after expiry and the Vertex resource self-expires.
type geminiCacheRegistry struct {
	mu sync.Mutex
	m  map[string]geminiCacheEntry
}

var geminiCaches = &geminiCacheRegistry{m: map[string]geminiCacheEntry{}}

// safetyMargin is subtracted from an entry's expiry when deciding whether it's
// still safe to reuse, guarding against the resource lapsing mid-request.
const geminiCacheSafetyMargin = 15 * time.Second

// lookup returns a still-valid resource name for the key, or "" on miss/expiry.
func (r *geminiCacheRegistry) lookup(key string, now time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[key]
	if !ok {
		return ""
	}
	if now.Add(geminiCacheSafetyMargin).After(e.expiry) {
		// Expired (or about to). Drop it so we re-create on the next call.
		delete(r.m, key)
		return ""
	}
	return e.name
}

// store records a freshly-created resource name with its expiry.
func (r *geminiCacheRegistry) store(key, name string, expiry time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[key] = geminiCacheEntry{name: name, expiry: expiry}
}

// geminiCacheKey derives a stable key for a system+tools prefix. Two requests
// with the SAME model, system instruction, and tool set share a cache resource
// (which is exactly when a cache hit is possible). Conversation contents are
// deliberately excluded — they change every turn, so including them would make
// every key unique and never hit.
func geminiCacheKey(model, systemPrompt string, tools []*genai.Tool) string {
	h := sha256.New()
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write([]byte(systemPrompt))
	h.Write([]byte{0})
	// Tool declarations are part of the cached prefix; fold a stable
	// serialization into the key so a tool-set change yields a new cache.
	if len(tools) > 0 {
		if b, err := json.Marshal(tools); err == nil {
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// MaybeApplyGeminiCache mutates `config` to reference an explicit CachedContent
// resource for the system instruction + tools prefix when the shared planner
// deems it worthwhile and the prefix clears the Gemini explicit-cache minimum.
//
// On a cache hit/create success it sets config.CachedContent to the resource
// name and CLEARS config.SystemInstruction / config.Tools / config.ToolConfig
// (they now live in the cache; re-sending them would be redundant and, for
// SystemInstruction, rejected by the API when CachedContent is set).
//
// On any miss/failure it leaves `config` unchanged so the caller proceeds with
// a normal uncached request. This function never returns an error — caching is
// strictly best-effort.
func (vc *VertexClient) MaybeApplyGeminiCache(ctx context.Context, modelName, systemPrompt string, contents []*genai.Content, config *genai.GenerateContentConfig, plan CachePlan) {
	if config == nil {
		return
	}

	prefixBytes := len(systemPrompt)
	var toolsBytes int
	if config.Tools != nil {
		if b, err := json.Marshal(config.Tools); err == nil {
			toolsBytes = len(b)
			prefixBytes += toolsBytes
		}
	}

	slog.DebugContext(ctx, "gemini cache evaluation",
		slog.Bool("planCacheSystem", plan.CacheSystem),
		slog.Int("systemPromptBytes", len(systemPrompt)),
		slog.Int("toolsBytes", toolsBytes),
		slog.Int("combinedBytes", prefixBytes),
		slog.Int("thresholdBytes", int(geminiCacheMinBytes)),
	)

	// Cheap gates first (no client needed): only proceed when the planner
	// judged the system prefix worth caching AND it clears the Gemini
	// explicit-cache minimum (higher than the inline-marker minimum), to avoid
	// a create the API would reject as too small.
	if !plan.CacheSystem {
		return
	}
	if int32(prefixBytes) < geminiCacheMinBytes {
		return
	}
	// Beyond this point we need a live client to create/reuse a resource.
	if vc.client == nil {
		return
	}

	fullName := FormatModelName(modelName)

	key := geminiCacheKey(fullName, systemPrompt, config.Tools)
	now := time.Now()

	// Fast path: reuse an existing, still-valid resource.
	if name := geminiCaches.lookup(key, now); name != "" {
		applyCachedContentToConfig(config, name)
		slog.DebugContext(ctx, "gemini cache hit", slog.String("resource", name))
		return
	}

	// Miss: create a new resource holding the system instruction + tools.
	ttl := time.Duration(geminiCacheTTLSecs) * time.Second
	createCfg := &genai.CreateCachedContentConfig{
		TTL:               ttl,
		SystemInstruction: config.SystemInstruction,
		Tools:             config.Tools,
		ToolConfig:        config.ToolConfig,
	}
	cc, err := vc.client.Caches.Create(ctx, fullName, createCfg)
	if err != nil || cc == nil || cc.Name == "" {
		// Best-effort: fall back to an uncached request.
		slog.WarnContext(ctx, "gemini cache create failed; proceeding uncached",
			slog.Any("error", err))
		return
	}

	// Track expiry locally so we know when to re-create. Prefer the server's
	// ExpireTime; fall back to now+TTL when it's not populated.
	expiry := cc.ExpireTime
	if expiry.IsZero() {
		expiry = now.Add(ttl)
	}
	geminiCaches.store(key, cc.Name, expiry)
	applyCachedContentToConfig(config, cc.Name)
	slog.DebugContext(ctx, "gemini cache created",
		slog.String("resource", cc.Name), slog.Time("expiry", expiry))
}

// applyCachedContentToConfig points the request at a cache resource and removes
// the now-redundant inline system instruction / tools (they're in the cache).
func applyCachedContentToConfig(config *genai.GenerateContentConfig, name string) {
	config.CachedContent = name
	config.SystemInstruction = nil
	config.Tools = nil
	config.ToolConfig = nil
}
