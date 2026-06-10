package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"
)

// logVertexTags scopes model-discovery fan-out logs to component=tags.
var logVertexTags = logx.Scoped("tags")

// supportedPublishers is the discoverable subset of publisherRegistry — the
// namespaces /api/tags fans out across. Derived from the registry so the list
// stays in sync automatically. Order is normalized for deterministic logs.
var supportedPublishers = discoverablePublishers()

// discoverablePublishers extracts and sorts the publisher namespaces marked
// discoverable in publisherRegistry.
func discoverablePublishers() []string {
	out := make([]string, 0, len(publisherRegistry))
	for name, info := range publisherRegistry {
		if info.discoverable {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ListModels returns the union of models exposed by Vertex AI across all
// supported publishers, including 3rd-party (Claude, Llama, Mistral, etc.)
// models.
//
// Discovery strategy per publisher:
//  1. project+location scoped endpoint (preferred so we honor regional access)
//  2. us-central1 fallback endpoint
//  3. unauthenticated-by-project global catalog (`/v1beta1/publishers/X/models`)
//
// Additionally any deployed/tuned models returned by the SDK's Models.List()
// are merged in. All failures are logged but never abort the overall list so
// that a single bad publisher doesn't blank out /api/tags.
func (vc *VertexClient) ListModels(ctx context.Context) ([]*genai.Model, error) {
	start := time.Now()

	type publisherResult struct {
		publisher string
		models    []*genai.Model
		source    string // which endpoint succeeded
		err       error
	}

	results := make(chan publisherResult, len(supportedPublishers))
	var wg sync.WaitGroup
	for _, p := range supportedPublishers {
		wg.Add(1)
		go func(pub string) {
			defer wg.Done()
			models, source, err := vc.fetchPublisherModels(ctx, pub)
			results <- publisherResult{publisher: pub, models: models, source: source, err: err}
		}(p)
	}
	wg.Wait()
	close(results)

	seen := make(map[string]bool)
	var combined []*genai.Model
	for r := range results {
		if r.err != nil {
			logVertexTags.Warnf("publisher=%s: discovery failed: %v", r.publisher, r.err)
			continue
		}
		added := 0
		for _, m := range r.models {
			baseName := lastSegment(m.Name)
			if baseName == "" || seen[baseName] {
				continue
			}
			if !isChatModel(m) {
				continue
			}
			combined = append(combined, m)
			seen[baseName] = true
			added++
		}
		logVertexTags.Infof("publisher=%s source=%s discovered=%d added=%d",
			r.publisher, r.source, len(r.models), added)
	}

	// Also pull in deployed/tuned models accessible to the project.
	deployed, derr := vc.listDeployedModels(ctx)
	if derr != nil {
		logVertexTags.Warnf("deployed models lookup failed: %v", derr)
	}
	deployedAdded := 0
	for _, m := range deployed {
		baseName := lastSegment(m.Name)
		if baseName == "" || seen[baseName] {
			continue
		}
		if !isChatModel(m) {
			continue
		}
		combined = append(combined, m)
		seen[baseName] = true
		deployedAdded++
	}
	if len(deployed) > 0 || derr == nil {
		logVertexTags.Infof("deployed discovered=%d added=%d", len(deployed), deployedAdded)
	}

	// Stable ordering makes the /api/tags output deterministic across calls.
	sort.SliceStable(combined, func(i, j int) bool {
		return combined[i].Name < combined[j].Name
	})

	logVertexTags.Infof("total models=%d publishers=%d elapsed=%v",
		len(combined), len(supportedPublishers), time.Since(start))
	return combined, nil
}

// fetchPublisherModels probes a chain of endpoints for the given publisher and
// returns the first non-empty result.
func (vc *VertexClient) fetchPublisherModels(ctx context.Context, publisher string) ([]*genai.Model, string, error) {
	endpoints := vc.publisherListEndpoints(publisher)

	var lastErr error
	for _, ep := range endpoints {
		models, err := vc.getModelsFromEndpoint(ctx, ep.url)
		if err != nil {
			lastErr = err
			logVertexTags.Warnf("publisher=%s endpoint=%s error: %v", publisher, ep.label, err)
			continue
		}
		if len(models) > 0 {
			return models, ep.label, nil
		}
		logVertexTags.Warnf("publisher=%s endpoint=%s returned 0 models", publisher, ep.label)
	}
	// If nothing failed but nothing returned either, that's still a success
	// (just no models exposed for this publisher in this account/region).
	if lastErr == nil {
		return nil, "empty", nil
	}
	return nil, "exhausted", lastErr
}

type endpoint struct {
	label string
	url   string
}

// publisherListEndpoints returns the ordered list of URLs to try for a given
// publisher. The unscoped global catalog endpoint is always last because it
// works without project quota but lists every publisher model (we still want
// to honor any project-scoped restrictions when possible).
func (vc *VertexClient) publisherListEndpoints(publisher string) []endpoint {
	var endpoints []endpoint
	if vc.projectID != "" && vc.location != "" {
		if vc.location == "global" {
			endpoints = append(endpoints, endpoint{
				label: "project-global",
				url: fmt.Sprintf(
					"https://aiplatform.googleapis.com/v1beta1/projects/%s/locations/global/publishers/%s/models",
					vc.projectID, publisher),
			})
		} else {
			endpoints = append(endpoints, endpoint{
				label: "project-regional",
				url: fmt.Sprintf(
					"https://%s-aiplatform.googleapis.com/v1beta1/projects/%s/locations/%s/publishers/%s/models",
					vc.location, vc.projectID, vc.location, publisher),
			})
		}

		// us-central1 has the broadest publisher coverage; use it as fallback
		// whenever the configured location isn't already us-central1.
		if vc.location != "us-central1" {
			endpoints = append(endpoints, endpoint{
				label: "project-us-central1",
				url: fmt.Sprintf(
					"https://us-central1-aiplatform.googleapis.com/v1beta1/projects/%s/locations/us-central1/publishers/%s/models",
					vc.projectID, publisher),
			})
		}
	}
	// Global catalog: lists every publisher model, regardless of project.
	endpoints = append(endpoints, endpoint{
		label: "global-catalog",
		url:   fmt.Sprintf("https://us-central1-aiplatform.googleapis.com/v1beta1/publishers/%s/models", publisher),
	})
	return endpoints
}

// getModelsFromEndpoint performs a single GET against a publisher-listing URL
// and decodes the result into genai.Model objects.
func (vc *VertexClient) getModelsFromEndpoint(ctx context.Context, url string) ([]*genai.Model, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if vc.projectID != "" {
		req.Header.Set("x-goog-user-project", vc.projectID)
	}
	resp, err := vc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		PublisherModels []map[string]any `json:"publisherModels"`
		Models          []map[string]any `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	rawEntries := append([]map[string]any{}, payload.PublisherModels...)
	rawEntries = append(rawEntries, payload.Models...)

	var out []*genai.Model
	for _, m := range rawEntries {
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		out = append(out, &genai.Model{
			Name:             name,
			SupportedActions: extractSupportedActions(m),
		})
	}
	return out, nil
}

// extractSupportedActions tries to read the "supportedActions" or
// "launchStage"/"supportedGenerationMethods" hints from a raw model payload.
// When the field is absent we leave SupportedActions empty and let
// isChatModel() apply heuristic fall-backs.
func extractSupportedActions(m map[string]any) []string {
	if v, ok := m["supportedActions"].([]any); ok {
		var acts []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				acts = append(acts, s)
			}
		}
		return acts
	}
	if v, ok := m["supportedGenerationMethods"].([]any); ok {
		var acts []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				acts = append(acts, s)
			}
		}
		return acts
	}
	return nil
}

// listDeployedModels queries the genai SDK for non-base (tuned / deployed)
// models that the project has access to.
func (vc *VertexClient) listDeployedModels(ctx context.Context) ([]*genai.Model, error) {
	queryBaseFalse := false
	config := &genai.ListModelsConfig{QueryBase: &queryBaseFalse}

	var collected []*genai.Model
	pager, err := vc.client.Models.List(ctx, config)
	if err != nil {
		return nil, err
	}
	collected = append(collected, pager.Items...)
	for pager.NextPageToken != "" {
		pager, err = vc.client.Models.List(ctx, &genai.ListModelsConfig{
			PageToken: pager.NextPageToken,
			QueryBase: &queryBaseFalse,
		})
		if err != nil {
			return collected, err
		}
		collected = append(collected, pager.Items...)
	}
	return collected, nil
}

// lastSegment returns the final '/'-delimited segment of a resource path.
func lastSegment(name string) string {
	if name == "" {
		return ""
	}
	idx := strings.LastIndex(name, "/")
	if idx < 0 {
		return name
	}
	return name[idx+1:]
}

func isChatModel(m *genai.Model) bool {
	if len(m.SupportedActions) > 0 {
		for _, action := range m.SupportedActions {
			a := strings.ToLower(action)
			if a == "generatecontent" || a == "predict" || a == "streamgeneratecontent" || a == "streamrawpredict" {
				return true
			}
		}
		return false
	}
	if m.OutputTokenLimit > 0 {
		return true
	}
	// Fall-back heuristic: surface anything that isn't obviously an embedding /
	// image / safety / vision-encoder model.
	name := strings.ToLower(m.Name)
	exclude := []string{"embed", "embedding", "imagen", "imagegeneration", "veo", "moderation", "tts", "stt", "speech"}
	for _, ex := range exclude {
		if strings.Contains(name, ex) {
			return false
		}
	}
	return true
}
