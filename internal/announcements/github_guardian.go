package announcements

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/akash-network/price-feed-monitor/internal/alerting"
	"github.com/akash-network/price-feed-monitor/internal/config"
	"github.com/akash-network/price-feed-monitor/internal/types"
)

// guardianSetFilePattern matches files like v6.prototxt in the canonical_sets directory.
// When Wormhole adds a new guardian set file before an on-chain rotation, this is the
// earliest possible programmatic advance-warning signal (observed 6 days early for 4→5).
var guardianSetFilePattern = regexp.MustCompile(`(?i)^v\d+\.prototxt$`)

// guardianPRKeywords are title substrings that indicate a PR may relate to a guardian
// set rotation. Checked case-insensitively.
var guardianPRKeywords = []string{
	"guardian set",
	"guardianset",
	"guardian rotation",
	"guardian upgrade",
}

const canonicalSetsPath = "guardianset/mainnetv2/canonical_sets"

type githubFile struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type githubPR struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// GitHubGuardianMonitor watches the wormhole-foundation/wormhole GitHub repository for
// two signals that may precede a guardian set rotation:
//
//  1. A new vN.prototxt file appearing in guardianset/mainnetv2/canonical_sets/ —
//     this was observed 6 days before the index 4→5 rotation (March 2026).
//
//  2. An open PR with a title matching guardian-rotation keywords — catches proposals
//     even before they are merged.
//
// Neither signal is guaranteed (the 5→6 rotation had no GitHub pre-announcement), but
// both are low-cost to monitor and provide genuine advance warning when present.
type GitHubGuardianMonitor struct {
	cfg          config.GitHubConfig
	alerter      alerting.Alerter
	logger       *slog.Logger
	client       *http.Client
	knownFiles   map[string]bool
	seenPRs      map[int]bool
	baselined    bool
}

func NewGitHubGuardianMonitor(cfg config.GitHubConfig, alerter alerting.Alerter, logger *slog.Logger) *GitHubGuardianMonitor {
	return &GitHubGuardianMonitor{
		cfg:        cfg,
		alerter:    alerter,
		logger:     logger.With("component", "github_guardian"),
		client:     &http.Client{Timeout: 15 * time.Second},
		knownFiles: make(map[string]bool),
		seenPRs:    make(map[int]bool),
	}
}

func (m *GitHubGuardianMonitor) Run(ctx context.Context) {
	m.logger.Info("github guardian monitor started",
		"repo", m.cfg.Repo,
		"poll_interval", m.cfg.PollInterval.Duration,
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

func (m *GitHubGuardianMonitor) check(ctx context.Context) {
	m.checkFiles(ctx)
	m.checkPRs(ctx)
	m.baselined = true
}

// checkFiles polls the canonical_sets directory for new vN.prototxt files.
func (m *GitHubGuardianMonitor) checkFiles(ctx context.Context) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s", m.cfg.Repo, canonicalSetsPath)
	files, err := m.fetchFiles(ctx, url)
	if err != nil {
		m.logger.Error("failed to fetch guardian set directory", "error", err)
		return
	}

	isFirst := !m.baselined
	for _, f := range files {
		if f.Type != "file" || !guardianSetFilePattern.MatchString(f.Name) {
			continue
		}
		if m.knownFiles[f.Name] {
			continue
		}
		m.knownFiles[f.Name] = true
		if isFirst {
			continue // baseline pass — don't alert on existing files
		}

		m.logger.Warn("new guardian set file detected in wormhole repo",
			"file", f.Name,
			"repo", m.cfg.Repo,
		)
		m.alerter.Send(types.Alert{
			Key:      "github_guardian_file_" + strings.TrimSuffix(f.Name, ".prototxt"),
			Severity: types.SeverityWarning,
			Title:    "GUARDIAN SET EARLY WARNING — NEW FILE IN WORMHOLE REPO",
			Body: fmt.Sprintf(
				"A new guardian set definition file has appeared in the Wormhole repository.\n\n"+
					"File: %s\n"+
					"Path: %s/%s\n"+
					"Repo: https://github.com/%s\n\n"+
					"Historical pattern: this file was committed 6 days before the index 4→5\n"+
					"rotation (March 2026). A rotation may be imminent.\n\n"+
					"Actions:\n"+
					"  1. Review the file contents for the new guardian addresses.\n"+
					"  2. Monitor Component 3 (Ethereum RPC) and Component 5 (Wormholescan)\n"+
					"     — they will alert the moment the rotation goes live.\n"+
					"  3. Prepare the submit_v_a_a command in advance so it can be run\n"+
					"     immediately when the rotation is detected.",
				f.Name,
				canonicalSetsPath, f.Name,
				m.cfg.Repo,
			),
		})
	}

	if isFirst {
		m.logger.Info("github guardian file baseline established", "known_files", len(m.knownFiles))
	}
}

// checkPRs polls open PRs for titles matching guardian rotation keywords.
func (m *GitHubGuardianMonitor) checkPRs(ctx context.Context) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/pulls?state=open&per_page=50&sort=updated", m.cfg.Repo)
	prs, err := m.fetchPRs(ctx, url)
	if err != nil {
		m.logger.Error("failed to fetch github PRs", "error", err)
		return
	}

	isFirst := !m.baselined
	for _, pr := range prs {
		if m.seenPRs[pr.Number] {
			continue
		}
		matched := matchPRKeywords(pr.Title)
		if len(matched) == 0 {
			continue
		}
		m.seenPRs[pr.Number] = true
		if isFirst {
			continue // baseline pass
		}

		m.logger.Warn("guardian-related PR detected in wormhole repo",
			"pr", pr.Number,
			"title", pr.Title,
			"matched", strings.Join(matched, ", "),
		)
		m.alerter.Send(types.Alert{
			Key:      fmt.Sprintf("github_guardian_pr_%d", pr.Number),
			Severity: types.SeverityWarning,
			Title:    "GUARDIAN SET EARLY WARNING — OPEN PR IN WORMHOLE REPO",
			Body: fmt.Sprintf(
				"An open pull request in the Wormhole repository may signal an upcoming\n"+
					"guardian set rotation.\n\n"+
					"PR #%d: %s\n"+
					"URL: %s\n"+
					"Opened: %s\n\n"+
					"Matched keywords: %s\n\n"+
					"Actions:\n"+
					"  1. Review the PR to assess whether a rotation is planned.\n"+
					"  2. Monitor Component 3 (Ethereum RPC) and Component 5 (Wormholescan)\n"+
					"     — they will alert the moment the rotation goes live.\n"+
					"  3. Prepare the submit_v_a_a command in advance so it can be run\n"+
					"     immediately when the rotation is detected.",
				pr.Number, pr.Title, pr.HTMLURL, pr.CreatedAt,
				strings.Join(matched, ", "),
			),
		})
	}

	if isFirst {
		m.logger.Info("github guardian PR baseline established", "open_prs_scanned", len(prs))
	}
}

func matchPRKeywords(title string) []string {
	lower := strings.ToLower(title)
	var matched []string
	for _, kw := range guardianPRKeywords {
		if strings.Contains(lower, kw) {
			matched = append(matched, kw)
		}
	}
	return matched
}

func (m *GitHubGuardianMonitor) fetchFiles(ctx context.Context, url string) ([]githubFile, error) {
	return githubFetch[[]githubFile](ctx, m.client, m.cfg.Token, url)
}

func (m *GitHubGuardianMonitor) fetchPRs(ctx context.Context, url string) ([]githubPR, error) {
	return githubFetch[[]githubPR](ctx, m.client, m.cfg.Token, url)
}

func githubFetch[T any](ctx context.Context, client *http.Client, token, url string) (T, error) {
	var zero T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return zero, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return zero, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("github API returned HTTP %d for %s", resp.StatusCode, url)
	}

	var result T
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return zero, fmt.Errorf("decode response from %s: %w", url, err)
	}
	return result, nil
}
