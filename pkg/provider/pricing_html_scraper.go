package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const htmlPricingURL = "https://cloud.google.com/gemini-enterprise-agent-platform/generative-ai/pricing"

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

func cleanHTML(s string) string {
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "<", "<")
	s = strings.ReplaceAll(s, ">", ">")
	return strings.TrimSpace(s)
}

var priceRegex = regexp.MustCompile(`\$([0-9.]+)`)

func parseFirstPrice(cell string) float64 {
	match := priceRegex.FindStringSubmatch(cell)
	if len(match) < 2 {
		return 0
	}
	var val float64
	fmt.Sscanf(match[1], "%f", &val)
	return val
}

var (
	claudeInputRe  = regexp.MustCompile(`(?i)Input:\s*\$([0-9.]+)`)
	claudeOutputRe = regexp.MustCompile(`(?i)Output:\s*\$([0-9.]+)`)
	claudeCacheRe  = regexp.MustCompile(`(?i)(Cache Hit|Cache Read):\s*\$([0-9.]+)`)
)

func parseClaudeCell(cell string) (input, output, cached float64) {
	if match := claudeInputRe.FindStringSubmatch(cell); len(match) > 1 {
		fmt.Sscanf(match[1], "%f", &input)
	}
	if match := claudeOutputRe.FindStringSubmatch(cell); len(match) > 1 {
		fmt.Sscanf(match[1], "%f", &output)
	}
	if match := claudeCacheRe.FindStringSubmatch(cell); len(match) > 1 {
		fmt.Sscanf(match[2], "%f", &cached)
	}
	return
}

// htmlCommentRe matches HTML comments so they can be removed before parsing.
var htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

// htmlH3Re matches a tier section heading on the pricing page, e.g.
// <h3 id="priority" ...>Priority</h3> or <h3 id="flexbatch" ...>Flex/Batch</h3>.
var htmlH3Re = regexp.MustCompile(`(?is)<h3\b[^>]*>(.*?)</h3>`)

// tierForOffset classifies a byte offset in the HTML into a routing tier by
// looking at the most recent <h3> heading before it. The live page renders one
// table per tier, each preceded by its tier heading; rows before any tier
// heading (or under a non-tier heading) are "standard".
//
// headings is a slice of (offset, tier) pairs sorted ascending by offset, where
// tier is "" for headings that don't name a known tier (treated as standard).
func tierForOffset(headings []struct {
	off  int
	tier string
}, off int) string {
	tier := ""
	for _, h := range headings {
		if h.off > off {
			break
		}
		tier = h.tier
	}
	if tier == "" {
		return "standard"
	}
	return tier
}

// tierSuffix maps a tier to the key suffix used in the pricing table. Standard
// rates live under the bare model key; Priority/Flex get a suffix so the
// estimator can look them up on demand. An unknown tier maps to standard.
func tierSuffix(tier string) string {
	switch tier {
	case "priority":
		return "priority"
	case "flex":
		return "flex"
	default:
		return ""
	}
}

// batchFlexCacheRe extracts the Batch sub-rate from a combined Flex/Batch cached
// cell such as "Batch: $0.075 (Global) Flex: $0.08 (Global)". The Batch rate is
// the canonical Flex/Batch cached discount we model.
var batchFlexCacheRe = regexp.MustCompile(`(?i)Batch:\s*\$([0-9.]+)`)

// parseTierCachePrice extracts the cached-input rate from a cell. For Flex/Batch
// cells that pack both "Batch:" and "Flex:" sub-rates it returns the Batch rate;
// otherwise it falls back to the first plain "$" price in the cell.
func parseTierCachePrice(cell string) float64 {
	if m := batchFlexCacheRe.FindStringSubmatch(cell); len(m) > 1 {
		var v float64
		fmt.Sscanf(m[1], "%f", &v)
		return v
	}
	return parseFirstPrice(cell)
}

func parseHTMLPricingContent(html string, table map[string]ModelPrice) error {
	// Strip HTML comments first: they can contain literal "<h3>" / "<tr>" text
	// (e.g. documentation snippets) that would otherwise corrupt the tier
	// section attribution below.
	html = htmlCommentRe.ReplaceAllString(html, "")

	trRe := regexp.MustCompile(`(?i)<tr>([\s\S]*?)</tr>`)
	tdRe := regexp.MustCompile(`(?i)<td[^>]*>([\s\S]*?)</td>`)

	// Pre-scan tier section headings so each row can be attributed to its tier.
	var headings []struct {
		off  int
		tier string
	}
	for _, loc := range htmlH3Re.FindAllStringSubmatchIndex(html, -1) {
		text := strings.ToLower(cleanHTML(html[loc[2]:loc[3]]))
		tier := ""
		switch {
		case strings.Contains(text, "priority"):
			tier = "priority"
		case strings.Contains(text, "flex"), strings.Contains(text, "batch"):
			tier = "flex"
		}
		headings = append(headings, struct {
			off  int
			tier string
		}{off: loc[0], tier: tier})
	}

	var currentModel string
	var currentTier string
	var currentModelPrice ModelPrice

	trMatches := trRe.FindAllStringSubmatchIndex(html, -1)
	for _, loc := range trMatches {
		trContent := html[loc[2]:loc[3]]
		rowTier := tierForOffset(headings, loc[0])

		tdMatches := tdRe.FindAllStringSubmatch(trContent, -1)
		if len(tdMatches) == 0 {
			continue
		}

		cells := make([]string, len(tdMatches))
		for i, tdMatch := range tdMatches {
			cells[i] = cleanHTML(tdMatch[1])
		}

		firstCell := cells[0]
		isModel := modelFamilyRe.MatchString(firstCell)

		if isModel {
			if len(cells) == 1 || (len(cells) > 1 && !strings.Contains(strings.ToLower(cells[1]), "input (") && !strings.Contains(strings.ToLower(cells[1]), "input:") && !strings.Contains(strings.ToLower(cells[1]), "output:")) {
				currentModel = normalizePriceKey(firstCell)
				currentTier = rowTier
				currentModelPrice = ModelPrice{Source: "HTML Scraper"}
			} else if len(cells) > 1 && (strings.Contains(strings.ToLower(cells[1]), "input:") || strings.Contains(strings.ToLower(cells[1]), "output:")) {
				// Claude-style single-cell layout. Tier-suffix the key so
				// Priority/Flex rows don't clobber the standard entry.
				modelKey := normalizePriceKey(firstCell) + tierSuffix(rowTier)
				input, output, cached := parseClaudeCell(tdMatches[1][1])
				if input > 0 || output > 0 {
					table[modelKey] = ModelPrice{
						InputPerM:  input,
						OutputPerM: output,
						CachedPerM: cached,
						Source:     "HTML Scraper",
					}
				}
				currentModel = ""
			}
		} else if currentModel != "" {
			lowerFirst := strings.ToLower(firstCell)
			if strings.Contains(lowerFirst, "input") {
				if len(cells) > 1 {
					currentModelPrice.InputPerM = parseFirstPrice(cells[1])
				}
				if len(cells) > 3 {
					// The Flex/Batch cached cell packs "Batch: $.. Flex: $..";
					// parseTierCachePrice picks the Batch sub-rate there and
					// falls back to the first "$" price for plain cells.
					currentModelPrice.CachedPerM = parseTierCachePrice(cells[3])
				}
			} else if strings.Contains(lowerFirst, "output") {
				if len(cells) > 1 {
					currentModelPrice.OutputPerM = parseFirstPrice(cells[1])
				}
				if currentModelPrice.InputPerM > 0 || currentModelPrice.OutputPerM > 0 {
					// Route to the tier-suffixed key. A model that lists no
					// price for this tier (all "N/A") yields 0/0 and is never
					// written, so tier-not-offered models get no tier key and
					// the estimator correctly falls back to standard.
					table[currentModel+tierSuffix(currentTier)] = currentModelPrice
				}
				currentModel = ""
			}
		}
	}
	return nil
}

func (vc *VertexClient) scrapeHTMLPricing(ctx context.Context, table map[string]ModelPrice) error {
	req, err := http.NewRequestWithContext(ctx, "GET", htmlPricingURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := vc.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	return parseHTMLPricingContent(string(body), table)
}
