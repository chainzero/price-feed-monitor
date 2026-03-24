package announcements

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/akash-network/price-feed-monitor/internal/alerting"
	"github.com/akash-network/price-feed-monitor/internal/config"
	"github.com/akash-network/price-feed-monitor/internal/types"
)

// rssItem maps to an <item> element in the Discourse RSS feed.
type rssItem struct {
	Title       string `xml:"title"`
	Description string `xml:"description"` // HTML content (CDATA)
	Link        string `xml:"link"`
	PubDate     string `xml:"pubDate"`
	GUID        string `xml:"guid"`
}

type rssFeed struct {
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

// PythForumMonitor polls the Pyth DAO forum RSS feed and alerts when new
// proposals match guardian-rotation-related keywords.
//
// On startup it silently records all existing items as a baseline so that
// a fresh deploy does not flood the channel with historical proposals.
// Only items published after the first successful poll trigger alerts.
type PythForumMonitor struct {
	cfg       config.PythForumConfig
	alerter   alerting.Alerter
	logger    *slog.Logger
	client    *http.Client
	seenGUIDs map[string]bool
	baselined bool
}

func NewPythForumMonitor(cfg config.PythForumConfig, alerter alerting.Alerter, logger *slog.Logger) *PythForumMonitor {
	return &PythForumMonitor{
		cfg:       cfg,
		alerter:   alerter,
		logger:    logger.With("component", "pyth_forum"),
		client:    &http.Client{Timeout: 15 * time.Second},
		seenGUIDs: make(map[string]bool),
	}
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (m *PythForumMonitor) Run(ctx context.Context) {
	m.logger.Info("pyth forum monitor started",
		"url", m.cfg.URL,
		"poll_interval", m.cfg.PollInterval.Duration,
		"keywords", m.cfg.Keywords,
	)

	m.check(ctx)

	ticker := time.NewTicker(m.cfg.PollInterval.Duration)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.check(ctx)
		}
	}
}

func (m *PythForumMonitor) check(ctx context.Context) {
	feed, err := m.fetchFeed(ctx)
	if err != nil {
		m.logger.Error("failed to fetch Pyth DAO forum RSS", "error", err)
		return
	}

	isFirstRun := !m.baselined
	newMatches := 0

	for _, item := range feed.Channel.Items {
		guid := item.GUID
		if guid == "" {
			guid = item.Link // fallback if guid is absent
		}
		if m.seenGUIDs[guid] {
			continue
		}
		m.seenGUIDs[guid] = true

		if isFirstRun {
			// Baseline pass — record without alerting.
			continue
		}

		if matched := m.matchKeywords(item); len(matched) > 0 {
			newMatches++
			m.sendAlert(item, matched)
		}
	}

	m.baselined = true

	if isFirstRun {
		m.logger.Info("pyth forum baseline established", "items_indexed", len(feed.Channel.Items))
	} else {
		m.logger.Debug("pyth forum check complete", "new_matching_proposals", newMatches)
	}
}

// matchKeywords checks whether any configured keyword appears in the item's
// title or description (case-insensitive). Returns the matched keywords.
func (m *PythForumMonitor) matchKeywords(item rssItem) []string {
	searchText := strings.ToLower(item.Title + " " + item.Description)
	var matched []string
	for _, kw := range m.cfg.Keywords {
		if strings.Contains(searchText, strings.ToLower(kw)) {
			matched = append(matched, kw)
		}
	}
	return matched
}

func (m *PythForumMonitor) sendAlert(item rssItem, matchedKeywords []string) {
	m.logger.Warn("guardian-related forum post detected",
		"title", item.Title,
		"link", item.Link,
		"matched_keywords", strings.Join(matchedKeywords, ", "),
	)

	// Use guid-based alert key so each unique post gets its own dedup entry.
	alertKey := "pyth_forum_" + guidToKey(item.GUID)

	m.alerter.Send(types.Alert{
		Key:      alertKey,
		Severity: types.SeverityWarning,
		Title:    "GUARDIAN ROTATION EARLY WARNING — PYTH DAO FORUM",
		Body: fmt.Sprintf(
			"A new Pyth DAO proposal may signal an upcoming Wormhole guardian set rotation.\n\n"+
				"Source: Pyth DAO Forum\n"+
				"Post: %s\n"+
				"URL: %s\n"+
				"Published: %s\n\n"+
				"Matched keywords: %s\n\n"+
				"Action:\n"+
				"  1. Read the proposal and assess if a guardian set rotation is planned.\n"+
				"  2. Monitor the Ethereum Wormhole contract — Component 3 will alert if the\n"+
				"     guardian set index changes.\n"+
				"  3. Prepare governance proposal template for quick submission when rotation occurs.\n"+
				"     See: hermes-relayer-setup-guide.md#wormhole-guardian-set-management",
			item.Title,
			item.Link,
			item.PubDate,
			strings.Join(matchedKeywords, ", "),
		),
	})
}

func (m *PythForumMonitor) fetchFeed(ctx context.Context) (*rssFeed, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "price-feed-monitor/1.0")

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", m.cfg.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from Pyth forum RSS", resp.StatusCode)
	}

	var feed rssFeed
	if err := xml.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return nil, fmt.Errorf("parse RSS feed: %w", err)
	}

	return &feed, nil
}

// guidToKey converts a Discourse guid (e.g. "forum.pyth.network-topic-2393")
// to a safe alert key string.
func guidToKey(guid string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(guid) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}
