package server

import "net/http"

func (a *API) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !a.authorizeUIRequest(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>MultiScan Control Plane</title>
  <style>
    :root {
      --bg: #f3f4ef;
      --panel: #fffef8;
      --ink: #1b1f23;
      --muted: #5f6670;
      --line: #d8dccf;
      --danger: #b42318;
      --ok: #067647;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: "IBM Plex Sans", "Segoe UI", Tahoma, sans-serif;
      color: var(--ink);
      background:
        radial-gradient(circle at 10% 5%, #ece8db 0 180px, transparent 200px),
        radial-gradient(circle at 85% 20%, #e3efe8 0 200px, transparent 220px),
        var(--bg);
    }
    .wrap { max-width: 1280px; margin: 24px auto 64px; padding: 0 16px; }
    h1 {
      margin: 0;
      font-size: 34px;
      letter-spacing: 0.02em;
      font-family: "Space Grotesk", "Avenir Next", sans-serif;
    }
    .sub { margin: 6px 0 20px; color: var(--muted); }
    .grid {
      display: grid;
      gap: 16px;
      grid-template-columns: 1fr;
    }
    @media (min-width: 980px) {
      .grid { grid-template-columns: 1fr 1fr; }
    }
    .panel {
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 14px;
      padding: 14px;
      box-shadow: 0 8px 18px rgba(26, 30, 32, 0.06);
      overflow: auto;
    }
    .panel h2 {
      margin: 4px 0 10px;
      font-size: 18px;
      font-family: "Space Grotesk", "Avenir Next", sans-serif;
    }
    form {
      display: grid;
      gap: 10px;
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    @media (max-width: 760px) {
      form { grid-template-columns: 1fr; }
    }
    label { display: grid; gap: 6px; font-size: 13px; color: var(--muted); }
    input {
      border: 1px solid #cfd5c2;
      border-radius: 8px;
      padding: 10px;
      font-size: 14px;
      background: #fff;
    }
    button {
      border: 0;
      border-radius: 10px;
      padding: 10px 12px;
      font-size: 13px;
      font-weight: 600;
      color: #fff;
      background: linear-gradient(90deg, #0f7b6c, #0c6e90);
      cursor: pointer;
    }
    #job-form button { grid-column: 1 / -1; }
    .notice { margin-top: 8px; min-height: 20px; font-size: 13px; color: var(--muted); }
    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 13px;
      min-width: 640px;
    }
    th, td {
      text-align: left;
      border-bottom: 1px solid #e7eadf;
      padding: 8px;
      white-space: nowrap;
      vertical-align: middle;
    }
    th { color: var(--muted); font-weight: 600; }
    .status {
      padding: 3px 8px;
      border-radius: 999px;
      font-size: 12px;
      font-weight: 600;
    }
    .queued { color: #5e3d00; background: #fff3db; }
    .in_progress { color: #0a617a; background: #dbf2ff; }
    .completed { color: var(--ok); background: #dff8e7; }
    .failed { color: var(--danger); background: #ffe4e8; }
    .busy { color: #0a617a; background: #dbf2ff; }
    .idle { color: var(--ok); background: #dff8e7; }
    .offline { color: #5a5f69; background: #e8ebf0; }
    .row-button {
      background: #153f47;
      font-size: 12px;
      padding: 6px 10px;
    }
    .meta {
      color: var(--muted);
      font-size: 13px;
      margin: 2px 0 10px;
    }
  </style>
</head>
<body>
  <div class="wrap">
    <h1>MultiScan Control Plane</h1>
    <p class="sub">Submit scan jobs, monitor agents, and inspect open-port results.</p>

    <div class="grid">
      <section class="panel">
        <h2>Create Job</h2>
        <form id="job-form">
          <label>Hostname (single target, optional)
            <input name="hostname" placeholder="scanme.nmap.org" />
          </label>
          <label>Start IP
            <input name="start_ip" value="127.0.0.1" />
          </label>
          <label>End IP
            <input name="end_ip" value="127.0.0.1" />
          </label>
          <label>Start Port
            <input name="start_port" type="number" value="20" min="1" max="65535" required />
          </label>
          <label>End Port
            <input name="end_port" type="number" value="1024" min="1" max="65535" required />
          </label>
          <label>Max Attempts
            <input name="max_attempts" type="number" value="3" min="1" max="20" required />
          </label>
          <label>Scan Mode
            <div style="display:flex;align-items:center;gap:8px;color:#1b1f23;">
              <input id="top1000" name="top_1000" type="checkbox" style="width:auto;" />
              <span>Top 1000 TCP ports (nmap-style)</span>
            </div>
          </label>
          <label>Top N Ports (optional)
            <input name="top_n" type="number" value="0" min="0" max="65535" />
          </label>
          <button type="submit">Submit Job</button>
        </form>
        <div id="notice" class="notice"></div>
      </section>

      <section class="panel">
        <h2>Agents</h2>
        <table>
          <thead>
            <tr>
              <th>Agent</th>
              <th>State</th>
              <th>Current Job</th>
              <th>Completed</th>
              <th>Failed</th>
              <th>Last Seen (UTC)</th>
            </tr>
          </thead>
          <tbody id="agents-body"></tbody>
        </table>
      </section>
    </div>

    <section class="panel" style="margin-top:16px;">
      <h2>Jobs</h2>
      <table>
        <thead>
          <tr>
            <th>ID</th>
            <th>Status</th>
            <th>Range</th>
            <th>Ports</th>
            <th>Open Ports</th>
            <th>Pending</th>
            <th>Active</th>
            <th>Completed</th>
            <th>Updated (UTC)</th>
            <th>Error</th>
            <th>Results</th>
          </tr>
        </thead>
        <tbody id="jobs-body"></tbody>
      </table>
    </section>

    <section class="panel" style="margin-top:16px;">
      <h2>Scan Results</h2>
      <p id="results-meta" class="meta">Select a job to view open ports ordered by IP then port.</p>
      <table>
        <thead>
          <tr>
            <th>IP</th>
            <th>Port</th>
          </tr>
        </thead>
        <tbody id="results-body"></tbody>
      </table>
    </section>
  </div>

  <script>
    const noticeEl = document.getElementById('notice');
    const jobsBody = document.getElementById('jobs-body');
    const agentsBody = document.getElementById('agents-body');
    const resultsBody = document.getElementById('results-body');
    const resultsMeta = document.getElementById('results-meta');
    let selectedJobID = '';

    function esc(v) {
      return String(v || '').replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');
    }

    function ts(v) {
      if (!v) return '-';
      const d = new Date(v);
      if (Number.isNaN(d.getTime())) return '-';
      return d.toISOString().replace('T', ' ').replace('Z', '');
    }

    function badge(value, cls) {
      return '<span class="status ' + cls + '">' + esc(value) + '</span>';
    }

    function portsLabel(j) {
      const count = Number(j.port_count || 0);
      const span = Number(j.end_port || 0) - Number(j.start_port || 0) + 1;
      if (count > 0 && span !== count) {
        return 'list(' + count + ')';
      }
      return esc(j.start_port) + '-' + esc(j.end_port);
    }

    async function loadJobs() {
      const res = await fetch('/api/jobs');
      const data = await res.json();
      const jobs = (data.jobs || []).slice().reverse();
      jobsBody.innerHTML = jobs.map(function(j) {
        return '<tr>' +
          '<td>' + esc(j.id) + '</td>' +
          '<td>' + badge(j.status, j.status) + '</td>' +
          '<td>' + esc(j.start_ip) + ' - ' + esc(j.end_ip) + '</td>' +
          '<td>' + portsLabel(j) + '</td>' +
          '<td>' + esc(j.open_port_count) + '</td>' +
          '<td>' + esc(j.sub_jobs_pending) + '</td>' +
          '<td>' + esc(j.sub_jobs_active) + '</td>' +
          '<td>' + esc(j.sub_jobs_completed) + '</td>' +
          '<td>' + ts(j.updated_at) + '</td>' +
          '<td>' + esc(j.last_error || '-') + '</td>' +
          '<td><button class="row-button" data-job-id="' + esc(j.id) + '">View</button></td>' +
          '</tr>';
      }).join('');
      if (!jobs.length) {
        jobsBody.innerHTML = '<tr><td colspan="11">No jobs yet.</td></tr>';
      }

      jobsBody.querySelectorAll('button[data-job-id]').forEach(function(btn) {
        btn.addEventListener('click', function() {
          selectedJobID = btn.getAttribute('data-job-id') || '';
          loadResults(selectedJobID);
        });
      });

      if (selectedJobID) {
        const exists = jobs.some(function(j) { return j.id === selectedJobID; });
        if (!exists) {
          selectedJobID = '';
          resultsMeta.textContent = 'Select a job to view open ports ordered by IP then port.';
          resultsBody.innerHTML = '<tr><td colspan="2">No job selected.</td></tr>';
        }
      }
    }

    async function loadAgents() {
      const res = await fetch('/api/agents');
      const data = await res.json();
      const agents = (data.agents || []).slice().sort(function(a, b) {
        return a.agent_id.localeCompare(b.agent_id);
      });
      agentsBody.innerHTML = agents.map(function(a) {
        return '<tr>' +
          '<td>' + esc(a.agent_id) + '</td>' +
          '<td>' + badge(a.state, a.state) + '</td>' +
          '<td>' + esc(a.current_job_id || '-') + '</td>' +
          '<td>' + esc(a.completed_jobs) + '</td>' +
          '<td>' + esc(a.failed_jobs) + '</td>' +
          '<td>' + ts(a.last_seen) + '</td>' +
          '</tr>';
      }).join('');
      if (!agents.length) agentsBody.innerHTML = '<tr><td colspan="6">No agents reported yet.</td></tr>';
    }

    async function loadResults(jobID) {
      if (!jobID) {
        resultsMeta.textContent = 'Select a job to view open ports ordered by IP then port.';
        resultsBody.innerHTML = '<tr><td colspan="2">No job selected.</td></tr>';
        return;
      }

      const res = await fetch('/work/' + encodeURIComponent(jobID));
      const body = await res.json();
      if (!res.ok) {
        resultsMeta.textContent = 'Failed to load results for ' + jobID;
        resultsBody.innerHTML = '<tr><td colspan="2">' + esc(body.error || 'Request failed') + '</td></tr>';
        return;
      }

      const results = body.results || [];
      resultsMeta.textContent = 'Job ' + jobID + ': ' + results.length + ' open endpoint(s), ordered by IP then port.';
      resultsBody.innerHTML = results.map(function(r) {
        return '<tr><td>' + esc(r.ip) + '</td><td>' + esc(r.port) + '</td></tr>';
      }).join('');
      if (!results.length) {
        resultsBody.innerHTML = '<tr><td colspan="2">No open ports recorded for this job.</td></tr>';
      }
    }

    async function refresh() {
      try {
        await Promise.all([loadJobs(), loadAgents()]);
        if (selectedJobID) {
          await loadResults(selectedJobID);
        }
      } catch (e) {
        noticeEl.textContent = 'Refresh error: ' + e.message;
        noticeEl.style.color = '#b42318';
      }
    }

    document.getElementById('job-form').addEventListener('submit', async function(e) {
      e.preventDefault();
      const fd = new FormData(e.target);
      const payload = {
        hostname: String(fd.get('hostname') || '').trim(),
        start_ip: String(fd.get('start_ip') || '').trim(),
        end_ip: String(fd.get('end_ip') || '').trim(),
        start_port: Number(fd.get('start_port')),
        end_port: Number(fd.get('end_port')),
        top_1000: Boolean(fd.get('top_1000')),
        top_n: Number(fd.get('top_n') || 0),
        max_attempts: Number(fd.get('max_attempts'))
      };

      noticeEl.textContent = 'Submitting job...';
      noticeEl.style.color = '#5f6670';

      const res = await fetch('/api/jobs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
      const body = await res.json();
      if (!res.ok) {
        noticeEl.textContent = body.error || 'Failed to submit job';
        noticeEl.style.color = '#b42318';
        return;
      }

      selectedJobID = body.job_id;
      if ((body.job_count || 0) > 1) {
        noticeEl.textContent = 'Jobs submitted: ' + body.job_count + ' batches (first ID: ' + body.job_id + ')';
      } else {
        noticeEl.textContent = 'Job submitted: ' + body.job_id;
      }
      noticeEl.style.color = '#067647';
      await refresh();
    });

    resultsBody.innerHTML = '<tr><td colspan="2">No job selected.</td></tr>';
    refresh();
    setInterval(refresh, 5000);
  </script>
</body>
</html>`
