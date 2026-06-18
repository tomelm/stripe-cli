package evals

import (
	"fmt"
	"html/template"
	"strings"
)

var htmlReportTemplate = template.Must(template.New("coop-eval-report").Funcs(template.FuncMap{
	"duration":         formatDurationMS,
	"score":            formatScore,
	"delta":            formatDelta,
	"statusClass":      statusClass,
	"findingClass":     findingClass,
	"checkStatus":      checkStatus,
	"verificationText": verificationText,
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root {
  --bg: #f7f7f5;
  --panel: #ffffff;
  --text: #202124;
  --muted: #63635f;
  --line: #d8d8d1;
  --line-strong: #b9b9af;
  --pass: #0f7b4b;
  --fail: #b42318;
  --warn: #9a6700;
  --info: #2454a6;
  --shadow: 0 1px 2px rgba(20, 20, 20, 0.06);
}
* { box-sizing: border-box; }
body {
  margin: 0;
  background: var(--bg);
  color: var(--text);
  font: 14px/1.45 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
}
header {
  border-bottom: 1px solid var(--line);
  background: #fff;
  position: sticky;
  top: 0;
  z-index: 10;
}
.wrap { max-width: 1260px; margin: 0 auto; padding: 24px; }
.topbar { display: flex; justify-content: space-between; gap: 16px; align-items: flex-end; }
h1 { margin: 0; font-size: 24px; line-height: 1.2; font-weight: 650; }
h2 { margin: 0 0 12px; font-size: 17px; line-height: 1.25; }
h3 { margin: 0 0 8px; font-size: 15px; line-height: 1.25; }
p { margin: 0 0 10px; }
code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
a { color: var(--info); text-decoration: none; }
a:hover { text-decoration: underline; }
.muted { color: var(--muted); }
.grid { display: grid; gap: 12px; }
.metrics { grid-template-columns: repeat(6, minmax(0, 1fr)); margin-top: 18px; }
.metric, .panel, .case-card, .timeline, .intro-card, .score-def, .eval-case {
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 8px;
  box-shadow: var(--shadow);
}
.metric { padding: 12px; min-width: 0; }
.metric .value { font-size: 22px; font-weight: 700; line-height: 1.1; }
.metric .label { margin-top: 4px; color: var(--muted); font-size: 12px; }
.section { margin-top: 22px; }
.panel { padding: 16px; }
.intro-grid { grid-template-columns: minmax(0, 1.05fr) minmax(0, 0.95fr); }
.intro-card { padding: 16px; }
.score-guide { grid-template-columns: repeat(4, minmax(0, 1fr)); }
.score-def { padding: 12px; }
.score-def code { display: block; margin-bottom: 4px; color: var(--muted); }
.score-def strong { display: block; margin-bottom: 4px; }
.eval-case-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
.eval-case { padding: 14px; min-width: 0; }
.eval-case h3 { margin-bottom: 6px; }
.case-meta { display: flex; flex-wrap: wrap; gap: 8px; margin: 10px 0; }
.latest-result { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin-top: 10px; }
.source-row { margin-top: 8px; }
.source-row a { font-weight: 650; }
.controls { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; margin: 16px 0; }
.controls input, .controls select {
  border: 1px solid var(--line-strong);
  border-radius: 6px;
  background: #fff;
  color: var(--text);
  padding: 8px 10px;
  font: inherit;
}
.controls input { min-width: 280px; flex: 1; }
.run-header { display: flex; justify-content: space-between; gap: 16px; align-items: flex-start; margin-bottom: 12px; }
.run-stats { display: flex; gap: 10px; flex-wrap: wrap; justify-content: flex-end; }
.pill {
  display: inline-flex;
  align-items: center;
  min-height: 24px;
  border: 1px solid var(--line);
  border-radius: 999px;
  padding: 3px 8px;
  color: var(--muted);
  background: #fafafa;
  font-size: 12px;
}
.pill.pass { color: var(--pass); border-color: #acd9c1; background: #f1fbf5; }
.pill.fail, .pill.blocking { color: var(--fail); border-color: #efb3ad; background: #fff6f5; }
.pill.major { color: var(--warn); border-color: #e7c36f; background: #fff9e8; }
.case-list { display: grid; gap: 12px; }
.case-card { padding: 14px; min-width: 0; }
.case-head { display: grid; grid-template-columns: minmax(0, 1fr); gap: 10px; align-items: start; }
.case-head > div { min-width: 0; }
.case-title { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.case-title h3 { margin: 0; }
.case-summary {
  border-left: 3px solid var(--line-strong);
  color: var(--text);
  margin: 8px 0;
  padding-left: 10px;
}
.artifact-links {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 8px;
}
.artifact-link {
  border: 1px solid var(--line);
  border-radius: 6px;
  color: var(--info);
  display: inline-flex;
  flex-direction: column;
  gap: 2px;
  min-width: 150px;
  padding: 7px 8px;
}
.artifact-link span { color: var(--muted); font-size: 12px; }
.score-row { display: flex; gap: 8px; flex-wrap: wrap; justify-content: flex-start; }
.score { font-variant-numeric: tabular-nums; }
.score.good { color: var(--pass); }
.score.bad { color: var(--fail); }
.score.warn { color: var(--warn); }
details { margin-top: 12px; }
summary { cursor: pointer; color: var(--info); font-weight: 600; }
.evidence-guide {
  background: #fafafa;
  border: 1px solid var(--line);
  border-radius: 6px;
  color: var(--muted);
  display: grid;
  gap: 6px;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  margin-top: 12px;
  padding: 10px;
}
.evidence-guide p { margin: 0; }
.evidence-guide strong { color: var(--text); }
.columns { display: grid; grid-template-columns: minmax(0, 1.1fr) minmax(0, 0.9fr); gap: 14px; margin-top: 12px; }
.subpanel { border-top: 1px solid var(--line); padding-top: 12px; min-width: 0; overflow-wrap: anywhere; }
.checks, .findings, .steps, .commands, .changes { display: grid; gap: 8px; margin: 0; padding: 0; list-style: none; }
.check, .finding, .step, .command, .change {
  border: 1px solid var(--line);
  border-radius: 6px;
  padding: 8px;
  background: #fcfcfb;
  min-width: 0;
  overflow-wrap: anywhere;
}
.check.fail { border-color: #efb3ad; background: #fff6f5; }
.check.pass { border-color: #acd9c1; background: #f1fbf5; }
.finding.blocking { border-color: #efb3ad; background: #fff6f5; }
.finding.major { border-color: #e7c36f; background: #fff9e8; }
.step.done { border-color: #acd9c1; }
.step.current, .step.pending { border-color: #c7d5ef; }
.step-head, .command { display: flex; justify-content: space-between; gap: 10px; align-items: flex-start; flex-wrap: wrap; }
.command > span:first-child { min-width: 0; overflow-wrap: anywhere; }
.impl { margin-top: 8px; color: var(--muted); }
code { overflow-wrap: anywhere; word-break: break-word; }
.snippet {
  margin: 8px 0 0;
  padding: 8px;
  overflow: auto;
  max-height: 220px;
  background: #f1f1ed;
  border-radius: 6px;
  border: 1px solid var(--line);
  white-space: pre-wrap;
}
table { width: 100%; border-collapse: collapse; font-variant-numeric: tabular-nums; }
th, td { border-bottom: 1px solid var(--line); padding: 8px; text-align: left; vertical-align: top; }
th { color: var(--muted); font-size: 12px; font-weight: 650; }
.timeline { padding: 14px; }
.timeline + .timeline { margin-top: 12px; }
.empty { color: var(--muted); font-style: italic; }
.hidden { display: none !important; }
@media (max-width: 900px) {
  .metrics { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .intro-grid, .score-guide, .eval-case-grid { grid-template-columns: 1fr; }
  .evidence-guide { grid-template-columns: 1fr; }
  .columns { grid-template-columns: 1fr; }
  .score-row, .run-stats { justify-content: flex-start; }
  .wrap { padding: 16px; }
}
@media (max-width: 640px) {
  .topbar { align-items: flex-start; }
  .timeline table, .timeline thead, .timeline tbody, .timeline tr, .timeline th, .timeline td {
    display: block;
    width: 100%;
  }
  .timeline thead { display: none; }
  .timeline tr {
    border-top: 1px solid var(--line);
    padding: 8px 0;
  }
  .timeline td {
    border-bottom: 0;
    padding: 4px 0;
  }
  .timeline td::before {
    color: var(--muted);
    content: attr(data-label);
    display: block;
    font-size: 12px;
    font-weight: 650;
    margin-bottom: 2px;
  }
}
</style>
</head>
<body>
<header>
  <div class="wrap topbar">
    <div>
      <h1>{{.Title}}</h1>
      <div class="muted">Generated {{.GeneratedAt.Format "2006-01-02 15:04:05 UTC"}}</div>
    </div>
    <div class="muted">{{.Summary.RunCount}} run{{if ne .Summary.RunCount 1}}s{{end}} · {{.Summary.CaseCount}} case{{if ne .Summary.CaseCount 1}}s{{end}}</div>
  </div>
</header>
<main class="wrap">
  <section class="grid metrics">
    <div class="metric"><div class="value">{{.Summary.PassedCases}}</div><div class="label">passed case results</div></div>
    <div class="metric"><div class="value">{{.Summary.FailedCases}}</div><div class="label">failed case results</div></div>
    <div class="metric"><div class="value">{{.Summary.JudgedCases}}</div><div class="label">LLM-judged cases</div></div>
    <div class="metric"><div class="value">{{.Summary.BlockingFindings}}</div><div class="label">blocking findings</div></div>
    <div class="metric"><div class="value">{{.Summary.MajorFindings}}</div><div class="label">major findings</div></div>
    <div class="metric"><div class="value">{{.Summary.RunCount}}</div><div class="label">runs included</div></div>
  </section>

  <section class="section grid intro-grid">
    <article class="intro-card">
      <h2>What This Eval Measures</h2>
      <p>Each case asks an AI agent to use the co-op CLI/TUI to implement a Stripe blueprint inside a fixture application. The harness records the co-op session, generated code, command checks, Stripe CLI usage, and an optional LLM judge review.</p>
      <p>A good result means the agent followed the co-op protocol, used the blueprint correctly, changed the existing app rather than building a detached demo, verified the behavior, and avoided unsafe eval behavior such as browser login, raw card numbers, or hardcoded keys.</p>
    </article>
    <article class="intro-card">
      <h2>How To Read Outcomes</h2>
      <p><strong>Status</strong> is the case-level verdict. <strong>Overall</strong> is the weighted deterministic harness score. <strong>LLM judge</strong> is an independent qualitative review of whether the result is product-ready for the app.</p>
      <p>Use failed checks and blocking judge findings to understand what broke. Use the evidence links on each case to inspect the session trace, judge verdict, generated diff, command log, and workspace snapshot.</p>
    </article>
  </section>

  <section class="section">
    <h2>Score Guide</h2>
    <div class="grid score-guide">
      {{range .ScoreGuide}}
      <article class="score-def">
        <code>{{.Name}}</code>
        <strong>{{.Label}}</strong>
        <p class="muted">{{.Description}}</p>
      </article>
      {{end}}
    </div>
  </section>

  {{if .EvalCases}}
  <section class="section">
    <h2>Eval Cases</h2>
    <p class="muted">These are the scenarios included in this report. Each case starts from a fixture app and asks the agent to implement one Stripe blueprint end-to-end through the co-op flow.</p>
    <div class="grid eval-case-grid">
      {{range .EvalCases}}
      <article class="eval-case">
        <h3><code>{{.ID}}</code></h3>
        <p>{{.Description}}</p>
        {{if .FixtureDescription}}<p class="muted">{{.FixtureDescription}}</p>{{end}}
        {{if .UpstreamURL}}
        <p class="source-row">
          Upstream app:
          <a href="{{.UpstreamURL}}">{{.UpstreamLabel}}</a>
          {{if .UpstreamRefURL}}<span class="muted">at</span> <a href="{{.UpstreamRefURL}}"><code>{{.UpstreamRef}}</code></a>{{end}}
        </p>
        {{end}}
        <div class="case-meta">
          {{if .Blueprint}}<span class="pill">Blueprint: {{.Blueprint}}</span>{{end}}
          {{if .Fixture}}<span class="pill">Fixture: {{.Fixture}}</span>{{end}}
          {{if .Language}}<span class="pill">Language: {{.Language}}</span>{{end}}
        </div>
        {{if .Tags}}
        <div class="case-meta">
          {{range .Tags}}<span class="pill">{{.}}</span>{{end}}
        </div>
        {{end}}
        <div class="latest-result">
          <span class="muted">Latest included run: <code>{{.LatestRun}}</code></span>
          <span class="pill {{statusClass .LatestStatus}}">{{.LatestStatus}}</span>
          <span class="pill">Overall {{score .OverallScore}}</span>
          {{if .HasJudge}}<span class="pill">LLM judge {{score .JudgeScore}}</span>{{end}}
        </div>
      </article>
      {{end}}
    </div>
  </section>
  {{end}}

  {{if .FixTimelines}}
  <section class="section">
    <h2>Fix Timeline</h2>
    {{range .FixTimelines}}
    <article class="timeline">
      <h3>{{.Title}}</h3>
      <p class="muted">{{.BeforeRun}} -> {{.AfterRun}}</p>
      {{if .Summary}}<p>{{.Summary}}</p>{{end}}
      {{if .Changes}}
      <ul class="changes">
        {{range .Changes}}<li class="change">{{.}}</li>{{end}}
      </ul>
      {{end}}
      {{if .Rows}}
      <table>
        <thead><tr><th>Case</th><th>Before</th><th>After</th><th>Overall</th><th>LLM judge</th></tr></thead>
        <tbody>
        {{range .Rows}}
          <tr>
            <td data-label="Case"><code>{{.CaseID}}</code></td>
            <td data-label="Before"><span class="pill {{statusClass .BeforeStatus}}">{{.BeforeStatus}}</span> {{score .BeforeOverall}}</td>
            <td data-label="After"><span class="pill {{statusClass .AfterStatus}}">{{.AfterStatus}}</span> {{score .AfterOverall}}</td>
            <td data-label="Overall">{{score .BeforeOverall}} -> {{score .AfterOverall}} <span class="score {{if ge .OverallDelta 0.0}}good{{else}}bad{{end}}">{{delta .OverallDelta}}</span></td>
            <td data-label="LLM judge">{{if .HasJudge}}{{score .BeforeJudge}} -> {{score .AfterJudge}} <span class="score {{if ge .JudgeDelta 0.0}}good{{else}}bad{{end}}">{{delta .JudgeDelta}}</span>{{else}}not judged{{end}}</td>
          </tr>
        {{end}}
        </tbody>
      </table>
      {{else}}
      <p class="empty">No matching case results were found for this fix annotation.</p>
      {{end}}
    </article>
    {{end}}
  </section>
  {{else if .ScoreMovements}}
  <section class="section panel">
    <h2>Score Movement</h2>
    <table>
      <thead><tr><th>Case</th><th>Runs</th><th>Overall</th><th>LLM judge</th></tr></thead>
      <tbody>
      {{range .ScoreMovements}}
        <tr>
          <td data-label="Case"><code>{{.CaseID}}</code></td>
          <td data-label="Runs">{{.BeforeRun}} -> {{.AfterRun}}</td>
          <td data-label="Overall">{{score .BeforeOverall}} -> {{score .AfterOverall}} <span class="score {{if ge .OverallDelta 0.0}}good{{else}}bad{{end}}">{{delta .OverallDelta}}</span></td>
          <td data-label="LLM judge">{{if .HasJudge}}{{score .BeforeJudge}} -> {{score .AfterJudge}} <span class="score {{if ge .JudgeDelta 0.0}}good{{else}}bad{{end}}">{{delta .JudgeDelta}}</span>{{else}}not judged{{end}}</td>
        </tr>
      {{end}}
      </tbody>
    </table>
  </section>
  {{end}}

  <section class="section">
    <h2>Runs</h2>
    <div class="controls">
      <input id="search" type="search" placeholder="Filter cases, blueprints, fixtures, findings">
      <select id="status-filter">
        <option value="all">All cases</option>
        <option value="failed">Failed cases</option>
        <option value="passed">Passed cases</option>
        <option value="judge">Judge failures</option>
      </select>
    </div>
    {{range .Runs}}
    <article class="panel section">
      <div class="run-header">
        <div>
          <h2>{{.ID}}</h2>
          <div class="muted"><code>{{.Path}}</code>{{if .Selection}} · {{.Selection}}{{end}}</div>
        </div>
        <div class="run-stats">
          <span class="pill {{if .Passed}}pass{{else}}fail{{end}}">{{if .Passed}}pass{{else}}fail{{end}}</span>
          <span class="pill">{{.PassedCases}}/{{.TotalCases}} cases passed</span>
          <span class="pill">overall {{score .AvgOverall}}</span>
          {{if .HasJudge}}<span class="pill">judge {{score .AvgJudge}}</span>{{end}}
          {{if .DurationMS}}<span class="pill">{{duration .DurationMS}}</span>{{end}}
          {{if .Interrupted}}<span class="pill fail">interrupted</span>{{end}}
        </div>
      </div>
      <div class="case-list">
      {{range .Cases}}
        <article class="case-card" data-status="{{if .Passed}}passed{{else}}failed{{end}}" data-judge="{{if and .Judge (not .Judge.Passed)}}failed{{else}}passed{{end}}" data-text="{{.SearchText}}">
          <div class="case-head">
            <div>
              <div class="case-title">
                <h3><code>{{.ID}}</code></h3>
                <span class="pill {{if .Passed}}pass{{else}}fail{{end}}">{{.Status}}</span>
                {{if .Blueprint}}<span class="pill">{{.Blueprint}}</span>{{end}}
                {{if .Fixture}}<span class="pill">{{.Fixture}}</span>{{end}}
              </div>
              {{if .Description}}<p class="muted">{{.Description}}</p>{{end}}
              {{if .Judge}}{{if .Judge.Summary}}<p class="case-summary"><strong>Judge summary:</strong> {{.Judge.Summary}}</p>{{end}}{{end}}
              <div class="score-row">
                {{range .Scores}}<span class="pill score" title="{{.Name}}">{{.Label}} {{score .Value}}</span>{{end}}
              </div>
              {{if .Artifacts}}
              <div class="artifact-links" aria-label="Evidence links">
                {{range .Artifacts}}
                <a class="artifact-link" href="{{.Href}}" title="{{.Path}}"><strong>{{.Label}}</strong><span>{{.Description}}</span></a>
                {{end}}
              </div>
              {{end}}
            </div>
          </div>
          <details>
            <summary>Why this result?</summary>
            <div class="evidence-guide">
              <p><strong>Outcome findings</strong> explain the verdict: failed deterministic checks, judge summary, and blocking or major issues.</p>
              <p><strong>Agent work log</strong> shows what the agent reported to co-op for each blueprint step. Treat it as implementation evidence, not the final verdict.</p>
              <p><strong>Recorded commands</strong> show what the harness, agent wrapper, smoke checks, and judge actually ran.</p>
            </div>
            <div class="columns">
              <div class="subpanel">
                <h3>Outcome Findings</h3>
                {{if .FailureReason}}<p class="pill fail">{{.FailureReason}}</p>{{end}}
                {{if .FailedChecks}}
                <ul class="checks">
                  {{range .FailedChecks}}
                  <li class="check fail"><strong>{{.Name}}</strong>{{if .Message}}: {{.Message}}{{end}}</li>
                  {{end}}
                </ul>
                {{end}}
                {{if .Judge}}
                  {{if .Judge.Summary}}<p>{{.Judge.Summary}}</p>{{end}}
                  {{if .Judge.Findings}}
                  <ul class="findings">
                    {{range .Judge.Findings}}
                    <li class="finding {{findingClass .Severity}}">
                      <div><span class="pill {{findingClass .Severity}}">{{.Severity}}</span> {{if .Category}}<span class="pill">{{.Category}}</span>{{end}}</div>
                      <p>{{.Message}}</p>
                      {{if .Evidence}}<p class="muted">{{.Evidence}}</p>{{end}}
                    </li>
                    {{end}}
                  </ul>
                  {{end}}
                {{end}}
                {{if and (not .FailedChecks) (not .Judge)}}<p class="empty">No failed checks or judge output recorded.</p>{{end}}
              </div>
              <div class="subpanel">
                <h3>Agent Work Log</h3>
                {{if .Session}}
                <ul class="steps">
                {{range .Session.Chapters}}
                  {{range .Nodes}}
                  <li class="step {{.State}}">
                    <div class="step-head"><strong>{{.Title}}</strong><span class="pill">{{.State}}</span></div>
                    <div class="muted"><code>{{.Key}}</code>{{if .Type}} · {{.Type}}{{end}}</div>
                    {{if .Implementation}}
                    <div class="impl">
                      {{if .Implementation.File}}<div>file: <code>{{.Implementation.File}}</code>{{if .Implementation.Lines}}:{{.Implementation.Lines}}{{end}}</div>{{end}}
                      {{if .Implementation.Note}}<p>{{.Implementation.Note}}</p>{{end}}
                      {{if .Implementation.Snippet}}<pre class="snippet">{{.Implementation.Snippet}}</pre>{{end}}
                    </div>
                    {{end}}
                    {{if .Verifications}}
                    <ul class="checks">
                      {{range .Verifications}}
                      <li class="check {{checkStatus .Passed}}">{{verificationText .Passed}} {{.Check}}</li>
                      {{end}}
                    </ul>
                    {{end}}
                  </li>
                  {{end}}
                {{end}}
                </ul>
                {{else}}
                <p class="empty">No final session artifact recorded.</p>
                {{end}}
              </div>
            </div>
            <div class="subpanel">
              <h3>Recorded Commands</h3>
              {{if .Commands}}
              <ul class="commands">
                {{range .Commands}}
                <li class="command"><span><code>{{.Name}}</code> <span class="muted">{{.Cwd}}</span></span><span class="pill {{if eq .ExitCode 0}}pass{{else}}fail{{end}}">exit {{.ExitCode}} · {{duration .DurationMS}}</span></li>
                {{end}}
              </ul>
              {{else}}
              <p class="empty">No command log recorded.</p>
              {{end}}
            </div>
          </details>
        </article>
      {{end}}
      </div>
    </article>
    {{end}}
  </section>
</main>
<script>
const search = document.getElementById('search');
const statusFilter = document.getElementById('status-filter');
function applyFilters() {
  const query = (search.value || '').trim().toLowerCase();
  const status = statusFilter.value;
  document.querySelectorAll('.case-card').forEach(card => {
    const matchesText = !query || card.dataset.text.includes(query);
    const matchesStatus =
      status === 'all' ||
      card.dataset.status === status ||
      (status === 'judge' && card.dataset.judge === 'failed');
    card.classList.toggle('hidden', !(matchesText && matchesStatus));
  });
}
search.addEventListener('input', applyFilters);
statusFilter.addEventListener('change', applyFilters);
</script>
</body>
</html>
`))

func formatScore(v float64) string {
	return fmt.Sprintf("%.2f", v)
}

func formatDelta(v float64) string {
	if v >= 0 {
		return fmt.Sprintf("+%.2f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

func statusClass(status string) string {
	switch {
	case strings.Contains(status, "pass"):
		return "pass"
	case strings.Contains(status, "fail"):
		return "fail"
	default:
		return ""
	}
}

func findingClass(severity string) string {
	switch strings.ToLower(severity) {
	case "blocking":
		return "blocking"
	case "major":
		return "major"
	default:
		return ""
	}
}

func checkStatus(passed bool) string {
	if passed {
		return "pass"
	}
	return "fail"
}

func verificationText(passed bool) string {
	if passed {
		return "PASS"
	}
	return "FAIL"
}
