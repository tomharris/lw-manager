package studio

import (
	"html/template"

	"github.com/tomharris/lw-manager/internal/corpus"
)

// screens is the recognizable game screen set this corpus is built for. The
// first six are the screens DefaultGraph navigates. alliance_members and
// vs_ranking are here because the recognizer must be able to *name* every
// screen the corpus asserts exists — a labelled frame with no identifying
// anchor is wrong on every scoring run, forever. Adding graph edges to them
// is M4 capture-route work and is deliberately not part of this.
var screens = []string{
	"base",
	"world_map",
	"alliance",
	"alliance_tech",
	"alliance_members",
	"vs_ranking",
	"mail",
	"radar",
}

// KnownLabels is every label an operator can assign a frame in the corpus:
// every real screen, the negatives bucket, and _unsorted itself — the last
// so a mislabelled frame can be moved back to _unsorted from the browser,
// which nothing else offers.
var KnownLabels = append(append([]string{}, screens...), corpus.None, corpus.Unsorted)

// AnchorScreens is the subset of KnownLabels a crop's "screen" field may
// name: real game screens only. corpus.None is the negatives bucket, not a
// screen — offering it here would let the recognizer *name* _none and mark
// every negative wrong. corpus.Unsorted frames have no anchors of their own
// either.
var AnchorScreens = screens

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

var cropTmpl = template.Must(template.New("crop").Parse(layout + `
<h1>crop an anchor</h1>
<p>drag a rectangle over the frame, then name the anchor.</p>
<div style="display:flex;gap:1rem;align-items:flex-start">
 <div style="position:relative;max-width:420px">
  <img id="f" src="/frame/{{.Frame.Hash}}" style="width:100%;display:block;user-select:none">
  <div id="sel" style="position:absolute;border:2px solid #8cf;background:rgba(136,204,255,.2);display:none"></div>
 </div>
 <form method="post" action="/crop" style="min-width:260px">
  <input type="hidden" name="hash" value="{{.Frame.Hash}}">
  <label>screen<select name="screen">{{range $.Screens}}<option value="{{.}}" {{if eq . $.Frame.Label}}selected{{end}}>{{.}}</option>{{end}}</select></label>
  <label>anchor id<input name="anchor_id" required pattern="[a-z0-9_]+"></label>
  <label>threshold<input name="threshold" type="number" step="0.01" min="0" max="1" value="0.85"></label>
  <label><input type="checkbox" name="identifies_screen" checked> identifies this screen</label>
  <input type="hidden" name="x1" id="x1"><input type="hidden" name="y1" id="y1">
  <input type="hidden" name="x2" id="x2"><input type="hidden" name="y2" id="y2">
  <button>cut template</button>
 </form>
</div>
<script>
// The rectangle is reported as fractions of the *displayed* image, never
// pixels, so a scaled-down view gives the same answer as a full-size one.
// That is what keeps invariant #1 true: no absolute coordinate leaves here.
(function () {
  const img = document.getElementById('f'), sel = document.getElementById('sel');
  let sx = 0, sy = 0, dragging = false;
  const frac = e => {
    const r = img.getBoundingClientRect();
    return [
      Math.min(Math.max((e.clientX - r.left) / r.width, 0), 1),
      Math.min(Math.max((e.clientY - r.top) / r.height, 0), 1),
    ];
  };
  img.addEventListener('mousedown', e => {
    e.preventDefault();
    [sx, sy] = frac(e); dragging = true; sel.style.display = 'block';
  });
  window.addEventListener('mousemove', e => {
    if (!dragging) return;
    const [cx, cy] = frac(e);
    const x1 = Math.min(sx, cx), y1 = Math.min(sy, cy);
    const x2 = Math.max(sx, cx), y2 = Math.max(sy, cy);
    sel.style.left = (x1 * 100) + '%'; sel.style.top = (y1 * 100) + '%';
    sel.style.width = ((x2 - x1) * 100) + '%'; sel.style.height = ((y2 - y1) * 100) + '%';
    document.getElementById('x1').value = x1.toFixed(5);
    document.getElementById('y1').value = y1.toFixed(5);
    document.getElementById('x2').value = x2.toFixed(5);
    document.getElementById('y2').value = y2.toFixed(5);
  });
  window.addEventListener('mouseup', () => { dragging = false; });
})();
</script>
`))
