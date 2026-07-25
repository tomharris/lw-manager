package studio

import (
	"html/template"

	"github.com/tomharris/lw-manager/internal/corpus"
)

// KnownLabels is the screen set this corpus is built for.
//
// The first six are the screens DefaultGraph navigates. alliance_members and
// vs_ranking are here because the recognizer must be able to *name* every
// screen the corpus asserts exists — a labelled frame with no identifying
// anchor is wrong on every scoring run, forever. Adding graph edges to them
// is M4 capture-route work and is deliberately not part of this.
var KnownLabels = []string{
	"base",
	"world_map",
	"alliance",
	"alliance_tech",
	"alliance_members",
	"vs_ranking",
	"mail",
	"radar",
	corpus.None,
}

const layout = `
<!doctype html>
<meta charset="utf-8">
<title>{{.Title}} — lw studio</title>
<style>
 body{font:14px/1.5 system-ui,sans-serif;margin:1rem;background:#111;color:#eee}
 a{color:#8cf} nav a{margin-right:1rem}
 .grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(180px,1fr));gap:1rem}
 .card{background:#1b1b1b;padding:.5rem;border-radius:6px}
 .card img{width:100%;height:auto;display:block;border-radius:4px}
 select,button{font:inherit;margin-top:.4rem;width:100%}
 h2{margin-top:2rem;border-bottom:1px solid #333}
 .count{color:#999}
</style>
<nav>
 <a href="/">unsorted</a><a href="/labeled">labeled</a>
 <form method="post" action="/capture" style="display:inline">
  <button style="width:auto" {{if not .CanCapture}}disabled title="no device attached"{{end}}>capture now</button>
 </form>
</nav>
`

var unsortedTmpl = template.Must(template.New("unsorted").Parse(layout + `
<h1>unsorted <span class="count">({{len .Frames}})</span></h1>
<div class="grid">
{{range .Frames}}
 <div class="card">
  <a href="/crop?hash={{.Hash}}"><img src="/frame/{{.Hash}}" alt="{{.Hash}}"></a>
  <form method="post" action="/label">
   <input type="hidden" name="hash" value="{{.Hash}}">
   <select name="label">{{range $.Labels}}<option value="{{.}}">{{.}}</option>{{end}}</select>
   <button>label</button>
  </form>
  <small>{{slice .Hash 0 12}}</small>
 </div>
{{end}}
</div>
`))

var labeledTmpl = template.Must(template.New("labeled").Parse(layout + `
<h1>labeled</h1>
{{range .Groups}}
 <h2>{{.Label}} <span class="count">({{len .Frames}})</span></h2>
 <div class="grid">
 {{range .Frames}}
  <div class="card">
   <a href="/crop?hash={{.Hash}}"><img src="/frame/{{.Hash}}" alt="{{.Hash}}"></a>
   <form method="post" action="/label">
    <input type="hidden" name="hash" value="{{.Hash}}">
    <select name="label">{{range $.Labels}}<option value="{{.}}">{{.}}</option>{{end}}</select>
    <button>move</button>
   </form>
  </div>
 {{end}}
 </div>
{{end}}
`))

// group is one label's frames on the labeled page.
type group struct {
	Label  string
	Frames []corpus.Frame
}
