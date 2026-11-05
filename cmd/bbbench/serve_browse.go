package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// handleBrowsePage serves the main results browsing interface.
func handleBrowsePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	html := `<!DOCTYPE html>
<html>
<head>
    <title>bbbench - Browse Results</title>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
        * { box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
            margin: 0;
            padding: 0;
            background: #f5f5f5;
        }
        __NAVBAR_CSS__
        .container {
            max-width: 1400px;
            margin: 0 auto;
            padding: 20px;
        }
        .controls {
            background: #fff;
            padding: 15px 20px;
            border-radius: 8px;
            margin-bottom: 20px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
            display: flex;
            gap: 15px;
            align-items: center;
        }
        .controls input, .controls select {
            padding: 8px 12px;
            border: 1px solid #ddd;
            border-radius: 4px;
            font-size: 14px;
        }
        .controls input {
            flex: 1;
            max-width: 300px;
        }
        .controls button {
            padding: 8px 16px;
            background: #2196f3;
            color: white;
            border: none;
            border-radius: 4px;
            cursor: pointer;
            font-size: 14px;
        }
        .controls button:hover {
            background: #1976d2;
        }
        .results-table {
            background: #fff;
            border-radius: 8px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
            overflow: hidden;
        }
        table {
            width: 100%;
            border-collapse: collapse;
        }
        th {
            background: #f5f5f5;
            padding: 12px 16px;
            text-align: left;
            font-weight: 600;
            color: #666;
            font-size: 13px;
            text-transform: uppercase;
            border-bottom: 2px solid #e0e0e0;
        }
        td {
            padding: 12px 16px;
            border-bottom: 1px solid #f0f0f0;
            font-size: 14px;
        }
        tr:hover {
            background: #f9f9f9;
        }
        .badge {
            display: inline-block;
            padding: 3px 8px;
            border-radius: 3px;
            font-size: 12px;
            font-weight: 500;
        }
        .badge-parallel {
            background: #e3f2fd;
            color: #1976d2;
        }
        .badge-sequential {
            background: #f3e5f5;
            color: #7b1fa2;
        }
        .actions a {
            color: #2196f3;
            text-decoration: none;
            margin-right: 12px;
            font-size: 13px;
        }
        .actions a:hover {
            text-decoration: underline;
        }
        .no-results {
            padding: 40px;
            text-align: center;
            color: #999;
        }
        .timestamp {
            color: #666;
            font-size: 13px;
        }
        .stats {
            color: #999;
            font-size: 12px;
        }
    </style>
</head>
<body>
__NAVBAR_HTML__
    <div class="container">
        <h2 style="margin: 20px 0 16px; color: #333; font-size: 20px;">Browse Benchmark Results</h2>
        <div class="controls">
            <input type="text" id="search" placeholder="Search by ID, mode, or drive...">
            <select id="filter-mode">
                <option value="">All Modes</option>
                <option value="parallel-sync">Parallel-Sync</option>
                <option value="sequential">Sequential</option>
            </select>
            <button onclick="refreshResults()">Refresh</button>
        </div>

        <div class="results-table">
            <table>
                <thead>
                    <tr>
                        <th>Timestamp</th>
                        <th>ID</th>
                        <th>Mode</th>
                        <th>Drives</th>
                        <th>Phases</th>
                        <th>Actions</th>
                    </tr>
                </thead>
                <tbody id="results-body">
                    <tr>
                        <td colspan="6" class="no-results">Loading results...</td>
                    </tr>
                </tbody>
            </table>
        </div>
    </div>

    <script>
        let allResults = [];

        function formatTimestamp(timestamp) {
            const date = new Date(timestamp);
            return date.toLocaleString();
        }

        function renderResults(results) {
            const tbody = document.getElementById('results-body');

            if (results.length === 0) {
                tbody.innerHTML = '<tr><td colspan="6" class="no-results">No results found</td></tr>';
                return;
            }

            tbody.innerHTML = results.map(result => {
                const modeBadge = result.mode === 'parallel-sync'
                    ? '<span class="badge badge-parallel">Parallel-Sync</span>'
                    : '<span class="badge badge-sequential">Sequential</span>';

                return '<tr>' +
                    '<td class="timestamp">' + formatTimestamp(result.timestamp) + '</td>' +
                    '<td><code>' + result.id + '</code></td>' +
                    '<td>' + modeBadge + '</td>' +
                    '<td class="stats">' + result.drive_count + ' drive(s)</td>' +
                    '<td class="stats">' + result.phase_count + ' phase(s)</td>' +
                    '<td class="actions">' +
                        '<a href="/result?id=' + result.id + '">Details</a>' +
                        '<a href="/summary?id=' + result.id + '">Summary</a>' +
                        '<a href="/graphs?id=' + result.id + '">Graphs</a>' +
                        '<a href="/compare?id=' + result.id + '">Compare</a>' +
                    '</td>' +
                    '</tr>';
            }).join('');
        }

        function filterResults() {
            const searchTerm = document.getElementById('search').value.toLowerCase();
            const modeFilter = document.getElementById('filter-mode').value;

            let filtered = allResults;

            // Apply search filter
            if (searchTerm) {
                filtered = filtered.filter(r =>
                    r.id.toLowerCase().includes(searchTerm) ||
                    r.mode.toLowerCase().includes(searchTerm) ||
                    String(r.drive_count).includes(searchTerm)
                );
            }

            // Apply mode filter
            if (modeFilter) {
                filtered = filtered.filter(r => r.mode === modeFilter);
            }

            renderResults(filtered);
        }

        function refreshResults() {
            fetch('/api/results')
                .then(response => response.json())
                .then(results => {
                    allResults = results;
                    filterResults();
                })
                .catch(error => {
                    console.error('Error loading results:', error);
                    document.getElementById('results-body').innerHTML =
                        '<tr><td colspan="6" class="no-results">Error loading results</td></tr>';
                });
        }

        // Set up event listeners
        document.getElementById('search').addEventListener('input', filterResults);
        document.getElementById('filter-mode').addEventListener('change', filterResults);

        // Load results on page load
        refreshResults();

        // Auto-refresh every 30 seconds
        setInterval(refreshResults, 30000);
    </script>
</body>
</html>`
	html = strings.ReplaceAll(html, "__NAVBAR_CSS__", navbarCSS)
	html = strings.ReplaceAll(html, "__NAVBAR_HTML__", navbarHTML("/results"))

	w.Write([]byte(html))
}

// handleResultDetailPage serves detailed information about a specific result.
func handleResultDetailPage(w http.ResponseWriter, r *http.Request) {
	resultID := r.URL.Query().Get("id")
	if resultID == "" {
		http.Error(w, "Missing result ID", http.StatusBadRequest)
		return
	}

	result, ok := globalResultsStore.GetResult(resultID)
	if !ok {
		http.Error(w, "Result not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
    <title>bbbench - Result Details</title>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
        * { box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
            margin: 0;
            padding: 0;
            background: #f5f5f5;
        }
        %s
        .container {
            max-width: 1400px;
            margin: 0 auto;
            padding: 20px;
        }
        .card {
            background: #fff;
            padding: 20px;
            border-radius: 8px;
            margin-bottom: 20px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
        }
        .card h2 {
            margin-top: 0;
            color: #333;
            font-size: 18px;
            border-bottom: 2px solid #f0f0f0;
            padding-bottom: 10px;
        }
        .info-grid {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
            gap: 15px;
            margin-top: 15px;
        }
        .info-item {
            padding: 12px;
            background: #f9f9f9;
            border-radius: 6px;
        }
        .info-label {
            font-size: 12px;
            color: #666;
            text-transform: uppercase;
            font-weight: 600;
        }
        .info-value {
            font-size: 16px;
            color: #333;
            margin-top: 5px;
        }
        .phase-list {
            list-style: none;
            padding: 0;
        }
        .phase-item {
            padding: 15px;
            background: #f9f9f9;
            border-left: 4px solid #2196f3;
            margin-bottom: 10px;
            border-radius: 4px;
        }
        .phase-name {
            font-weight: 600;
            color: #333;
            margin-bottom: 5px;
        }
        .phase-stats {
            font-size: 13px;
            color: #666;
        }
        .actions {
            margin-top: 20px;
        }
        .btn {
            display: inline-block;
            padding: 10px 20px;
            background: #2196f3;
            color: white;
            text-decoration: none;
            border-radius: 4px;
            margin-right: 10px;
            font-size: 14px;
        }
        .btn:hover {
            background: #1976d2;
        }
        .btn-secondary {
            background: #757575;
        }
        .btn-secondary:hover {
            background: #616161;
        }
    </style>
</head>
<body>
%s
    <div class="container">
        <div class="card">
            <h2>Overview</h2>
            <div class="info-grid">
                <div class="info-item">
                    <div class="info-label">Result ID</div>
                    <div class="info-value"><code>%s</code></div>
                </div>
                <div class="info-item">
                    <div class="info-label">Timestamp</div>
                    <div class="info-value">%s</div>
                </div>
                <div class="info-item">
                    <div class="info-label">Mode</div>
                    <div class="info-value">%s</div>
                </div>
                <div class="info-item">
                    <div class="info-label">Total Phases</div>
                    <div class="info-value">%d</div>
                </div>
            </div>

            <div class="actions">
                <a href="/summary?id=%s" class="btn">Summary</a>
                <a href="/graphs?id=%s" class="btn">Graphs</a>
                <a href="/compare?id=%s" class="btn">Compare</a>
                <a href="/export?id=%s" class="btn btn-secondary">Export HTML</a>
            </div>
        </div>

        <div class="card">
            <h2>Phases</h2>
            <ul class="phase-list" id="phases-list">
                <li>Loading phase details...</li>
            </ul>
        </div>
    </div>

    <script>
        const resultID = '%s';

        // Fetch full result details
        fetch('/api/results/detail?id=' + resultID)
            .then(response => response.json())
            .then(result => {
                const phasesList = document.getElementById('phases-list');

                if (!result.phases || result.phases.length === 0) {
                    phasesList.innerHTML = '<li style="padding: 20px; text-align: center; color: #999;">No phase data available</li>';
                    return;
                }

                phasesList.innerHTML = result.phases.map((phase, i) => {
                    const jobCount = phase.jobs ? phase.jobs.length : 0;
                    const duration = phase.duration_seconds ? phase.duration_seconds.toFixed(1) : 'N/A';

                    const esc = t => String(t).replace(/&/g, '&amp;').replace(/</g, '&lt;');
                    const skipped = Object.entries(phase.skipped || {}).map(([dev, why]) =>
                        '<div class="phase-stats" style="color:#1e3a5f;background:#e0ecff;border-radius:4px;padding:4px 8px;margin-top:4px;">' +
                        '<b>Skipped</b> ' + esc(why) + '</div>').join('');
                    const warned = Object.entries(phase.warnings || {}).map(([dev, why]) =>
                        '<div class="phase-stats" style="color:#8a5300;background:#fff3e0;border-radius:4px;padding:4px 8px;margin-top:4px;">' +
                        '&#9888; ' + esc(why) + '</div>').join('');
                    return '<li class="phase-item">' +
                        '<div class="phase-name">Phase ' + (i + 1) + ': ' + esc(phase.phase_name || 'Unnamed') + '</div>' +
                        '<div class="phase-stats">Jobs: ' + jobCount + ' | Duration: ' + duration + 's</div>' +
                        warned + skipped + '</li>';
                }).join('');
            })
            .catch(error => {
                console.error('Error loading result details:', error);
                document.getElementById('phases-list').innerHTML =
                    '<li style="padding: 20px; text-align: center; color: #f44336;">Error loading phase details</li>';
            });
    </script>
</body>
</html>`,
		navbarCSS,
		navbarHTML("/results"),
		escapeHTML(result.ID),
		result.Timestamp.Format("2006-01-02 15:04:05"),
		escapeHTML(result.Mode),
		len(result.Phases),
		escapeHTML(result.ID),
		escapeHTML(result.ID),
		escapeHTML(result.ID),
		escapeHTML(result.ID),
		escapeHTML(result.ID))

	w.Write([]byte(html))
}

// handleResultDetail serves detailed JSON data for a specific result.
func handleResultDetail(w http.ResponseWriter, r *http.Request) {
	resultID := r.URL.Query().Get("id")
	if resultID == "" {
		http.Error(w, "Missing result ID", http.StatusBadRequest)
		return
	}

	result, ok := globalResultsStore.GetResult(resultID)
	if !ok {
		http.Error(w, "Result not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		if logger != nil {
			logger.Error("encode result detail", "err", err)
		}
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
}

// escapeHTML escapes special HTML characters.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	return s
}
