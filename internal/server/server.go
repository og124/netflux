// Package server serves the status page, /status, /metrics and /sync.
package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"

	"github.com/og124/netflux/internal/controller"
)

// Handler returns the HTTP handler for c.
func Handler(c *controller.Controller) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(c.Status()) //nolint:errcheck
	})
	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, _ *http.Request) {
		c.SyncNow()
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintln(w, "sync queued")
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		writeMetrics(w, c.Status())
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page.Execute(w, c.Status()) //nolint:errcheck
	})
	return mux
}

func writeMetrics(w http.ResponseWriter, s controller.Status) {
	type metric struct {
		name, help, kind string
		value            func(controller.DeviceStatus) float64
	}
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	metrics := []metric{
		{"netflux_drift_leaves", "Leaves changed on the device since the last apply.", "gauge",
			func(d controller.DeviceStatus) float64 { return float64(len(d.Drift)) }},
		{"netflux_unconverged_leaves", "Leaves git sets that the device did not take.", "gauge",
			func(d controller.DeviceStatus) float64 { return float64(len(d.Unconverged)) }},
		{"netflux_pending_leaves", "Leaves an apply would change (observe mode).", "gauge",
			func(d controller.DeviceStatus) float64 { return float64(len(d.Pending)) }},
		{"netflux_streaming", "Whether the gNMI subscription is up.", "gauge",
			func(d controller.DeviceStatus) float64 { return b2f(d.Streaming) }},
		{"netflux_error", "Whether the last operation on the device failed.", "gauge",
			func(d controller.DeviceStatus) float64 { return b2f(d.LastError != "") }},
		{"netflux_applies_total", "Successful applies.", "counter",
			func(d controller.DeviceStatus) float64 { return float64(d.Applies) }},
		{"netflux_failures_total", "Failed operations.", "counter",
			func(d controller.DeviceStatus) float64 { return float64(d.Failures) }},
		{"netflux_heals_total", "Drift reverted in strict mode.", "counter",
			func(d controller.DeviceStatus) float64 { return float64(d.Heals) }},
		{"netflux_last_apply_timestamp_seconds", "When git was last applied.", "gauge",
			func(d controller.DeviceStatus) float64 {
				if d.LastApply.IsZero() {
					return 0
				}
				return float64(d.LastApply.Unix())
			}},
	}
	for _, m := range metrics {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.kind)
		for _, d := range s.Devices {
			fmt.Fprintf(w, "%s{device=%q,mode=%q} %g\n", m.name, d.Name, d.Mode, m.value(d))
		}
	}
	fmt.Fprintf(w, "# HELP netflux_desired_state_valid Whether the latest revision loaded cleanly.\n# TYPE netflux_desired_state_valid gauge\n")
	fmt.Fprintf(w, "netflux_desired_state_valid{revision=%q} %g\n", s.Revision, b2f(s.LoadError == ""))
}

var page = template.Must(template.New("status").Funcs(template.FuncMap{
	"since": func(s controller.DeviceStatus) string { return s.LastCheck.Format("15:04:05") },
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="2"><title>netflux</title>
<style>
:root{--bg:#fafaf9;--fg:#1c1917;--mute:#78716c;--line:#e7e5e4;--ok:#15803d;--bad:#b91c1c;--warn:#a16207;--card:#fff}
@media (prefers-color-scheme:dark){:root{--bg:#0c0a09;--fg:#e7e5e4;--mute:#a8a29e;--line:#292524;--ok:#4ade80;--bad:#f87171;--warn:#facc15;--card:#1c1917}}
body{margin:0;padding:24px 16px;background:var(--bg);color:var(--fg);font:14px/1.5 ui-sans-serif,system-ui,sans-serif}
main{max-width:960px;margin:0 auto}h1{font-size:20px;margin:0 0 4px}.mute{color:var(--mute)}
.card{background:var(--card);border:1px solid var(--line);border-radius:8px;padding:16px;margin:16px 0}
.row{display:flex;flex-wrap:wrap;gap:8px 24px;align-items:baseline}h2{font-size:16px;margin:0}
code,pre{font:12px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace}pre{margin:8px 0 0;white-space:pre-wrap;word-break:break-all}
.ok{color:var(--ok)}.bad{color:var(--bad)}.warn{color:var(--warn)}ul{margin:8px 0 0;padding-left:18px}
</style></head><body><main>
<h1>netflux</h1>
<div class="mute">git <code>{{.Revision}}</code>{{if .LoadError}} · <span class="bad">desired state invalid: {{.LoadError}}</span>{{end}}</div>
{{range .Devices}}<div class="card">
<div class="row"><h2>{{.Name}}</h2><code class="mute">{{.Address}}</code><span>mode <b>{{.Mode}}</b></span>
{{if .LastError}}<span class="bad">error</span>{{else if .Drift}}<span class="warn">drifted ({{len .Drift}})</span>{{else if .Pending}}<span class="warn">{{len .Pending}} pending</span>{{else}}<span class="ok">in sync</span>{{end}}
<span class="mute">{{if .Streaming}}streaming{{else}}not streaming{{end}} · applied <code>{{.Revision}}</code> · checked {{since .}}</span></div>
{{if .LastError}}<pre class="bad">{{.LastError}}</pre>{{end}}
{{if .DriftPR}}<div>drift PR: <a href="{{.DriftPR}}">{{.DriftPR}}</a></div>{{end}}
{{if .Drift}}<pre>{{range .Drift}}{{.}}
{{end}}</pre>{{end}}
{{if .Pending}}<div class="mute">would change:</div><pre>{{range .Pending}}{{.}}
{{end}}</pre>{{end}}
{{if .Unconverged}}<div class="warn">git sets these but the device didn't take them:</div><pre>{{range .Unconverged}}{{.}}
{{end}}</pre>{{end}}
<ul class="mute">{{range .Events}}<li><code>{{.Time.Format "15:04:05"}}</code> {{.Msg}}</li>{{end}}</ul>
</div>{{end}}
</main></body></html>`))
