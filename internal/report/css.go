package report

const reportCSS = `
:root { --ink:#1b1b1b; --muted:#666; --line:#d9d9d9; --soft:#f4f4f2; --accent:#1f4fa8; --mark:#fff3b0; }
* { box-sizing: border-box; }
body { margin:0; background:#e9e9e6; color:var(--ink); font: 11pt/1.45 Georgia, "Times New Roman", serif; }
.toolbar { position:sticky; top:0; background:#1f1f21; color:#fff; padding:10px 16px; display:flex; gap:10px;
  align-items:center; flex-wrap:wrap; font: 14px -apple-system, system-ui, sans-serif; z-index:2; }
.toolbar button, .toolbar a { font:inherit; font-weight:600; border:0; border-radius:8px; padding:8px 14px;
  background:#fff; color:#1f1f21; cursor:pointer; text-decoration:none; }
.toolbar button.primary { background:#6d9cff; color:#0b1020; }
.toolbar span { color:#bbb; font-size:13px; }
.page { background:#fff; max-width:8.5in; margin:16px auto; padding:0.8in 0.8in; box-shadow:0 1px 4px rgba(0,0,0,.15); }
h1 { font-size:24pt; margin:0 0 4pt; line-height:1.15; }
h2 { font-size:14pt; margin:22pt 0 8pt; padding-bottom:4pt; border-bottom:1.5px solid var(--ink); break-after:avoid; }
h3 { font-size:12pt; margin:14pt 0 4pt; break-after:avoid; }
h4 { font-size:11pt; margin:10pt 0 2pt; break-after:avoid; }
.sub { color:var(--muted); font-size:12pt; margin:0 0 24pt; }
.sans { font-family: -apple-system, "Helvetica Neue", Arial, sans-serif; }
table { width:100%; border-collapse:collapse; }
.facts td { padding:5pt 0; border-bottom:1px solid var(--line); vertical-align:top; }
.facts td:first-child { width:30%; color:var(--muted); font: 9.5pt -apple-system, "Helvetica Neue", Arial, sans-serif;
  text-transform:uppercase; letter-spacing:.04em; padding-top:7pt; }
.mono { font-family: ui-monospace, Menlo, monospace; font-size:8.5pt; word-break:break-all; }
.note { font-size:10pt; color:var(--muted); }
.ai { border:1.5px solid var(--ink); padding:10pt 14pt; margin-top:6pt; }
.ai-label { font: 700 9pt -apple-system, "Helvetica Neue", Arial, sans-serif; text-transform:uppercase; letter-spacing:.06em;
  border-bottom:1px solid var(--line); padding-bottom:6pt; margin-bottom:4pt; }
.ai-label span { display:block; font-weight:400; text-transform:none; letter-spacing:0; color:var(--muted); margin-top:3pt; }
.ai ul { padding-left:16pt; margin:4pt 0; } .ai li { margin:3pt 0; }
a.ref { color:var(--accent); text-decoration:none; font: 600 9pt -apple-system, "Helvetica Neue", Arial, sans-serif; }
.index th, .index td { text-align:left; padding:4pt 6pt 4pt 0; border-bottom:1px solid var(--line); vertical-align:top;
  font-size:9.5pt; }
.index th { font: 700 8.5pt -apple-system, "Helvetica Neue", Arial, sans-serif; text-transform:uppercase; color:var(--muted); }
.index td:nth-child(1) { white-space:nowrap; } .index td:nth-child(2) { white-space:nowrap; width:1%; }
.excerpt { margin-top:16pt; }
.excerpt-head { font: 600 9.5pt -apple-system, "Helvetica Neue", Arial, sans-serif; background:var(--soft);
  padding:5pt 8pt; border-left:3px solid var(--ink); break-after:avoid; }
.msg { display:grid; grid-template-columns: 0.72in 1.45in 1fr; gap:0 8pt; padding:4pt 0 4pt 8pt;
  border-bottom:1px solid #eee; break-inside:avoid; font-size:10pt; }
.msg .r { font: 600 8.5pt -apple-system, "Helvetica Neue", Arial, sans-serif; color:var(--muted); padding-top:1pt; }
.msg .t { font: 8.5pt -apple-system, "Helvetica Neue", Arial, sans-serif; color:var(--muted); padding-top:1pt; }
.msg .who { font-weight:700; }
.msg .body { white-space:pre-wrap; overflow-wrap:anywhere; }
.msg .att { font: italic 9pt Georgia, serif; color:var(--muted); }
.msg.match { background:var(--mark); border-left:3px solid #b08800; padding-left:5pt; }
.msg.match .r::after { content:" ★"; color:#8a6a00; }
.legend { font: 9pt -apple-system, "Helvetica Neue", Arial, sans-serif; color:var(--muted); margin:4pt 0 0; }
.legend b { background:var(--mark); border-left:3px solid #b08800; padding:0 4pt; font-weight:600; color:var(--ink); }
.cover { min-height:8.4in; display:flex; flex-direction:column; }
.cover .end { margin-top:auto; font-size:9.5pt; color:var(--muted); }
.pb { break-before:page; }
@page { size: letter; margin: 0.7in 0.75in;
  @bottom-center { content: "Page " counter(page) " of " counter(pages); font: 8.5pt Georgia, serif; color:#666; }
  @top-right { content: "REPORT_TITLE"; font: 8.5pt Georgia, serif; color:#666; } }
@page :first { @top-right { content: none; } }
@media print {
  body { background:#fff; }
  .toolbar { display:none; }
  .page { margin:0; padding:0; box-shadow:none; max-width:none; }
  a.ref { color:var(--ink); }
  .msg.match { -webkit-print-color-adjust:exact; print-color-adjust:exact; }
  .excerpt-head, .legend b { -webkit-print-color-adjust:exact; print-color-adjust:exact; }
}
`
