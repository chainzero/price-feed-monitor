// Package akashclient provides a shared HTTP fetch helper that tries multiple
// Akash API node URLs in order, falling back on network errors or 5xx responses.
package akashclient

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
)

// Fetch sends a GET request for path against each node in nodes, returning the
// first response that is not a network error and not a 5xx status.
//
// Callers are responsible for closing the returned response body.
// Returns an error only when every node in the list has failed.
func Fetch(ctx context.Context, client *http.Client, nodes []string, path string) (*http.Response, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no akash API nodes configured")
	}

	var lastErr error
	for i, node := range nodes {
		url := node + path
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			// Malformed URL — no point retrying other nodes with the same path.
			return nil, fmt.Errorf("build request for %s: %w", url, err)
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("node %s: %w", node, err)
			if i < len(nodes)-1 {
				slog.Warn("akash API node unreachable, trying next",
					"node", node,
					"path", path,
					"error", err,
				)
			}
			continue
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("node %s: status %d", node, resp.StatusCode)
			if i < len(nodes)-1 {
				slog.Warn("akash API node returned server error, trying next",
					"node", node,
					"path", path,
					"status", resp.StatusCode,
				)
			}
			continue
		}

		if i > 0 {
			slog.Info("akash API request succeeded on fallback node",
				"node", node,
				"path", path,
			)
		}
		return resp, nil
	}

	return nil, fmt.Errorf("all akash API nodes failed for %s: %w", path, lastErr)
}
