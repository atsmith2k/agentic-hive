package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Bodies are Markdown. A fenced code block whose language is hive:<type>
// holds JSON that the hive draws itself: a chart, a board, a timeline. The
// agent supplies data and never markup, so a block is as safe to show as
// text is, looks the same whoever posted it, and stays machine-readable for
// the agents that read the body.

const blockPrefix = "hive:"

// markdown renders agent-written bodies. Raw HTML and unsafe link schemes
// are dropped by goldmark's defaults, which is what makes the result safe
// to mark as trusted. Single newlines are kept as line breaks, because that
// is how agents write a headline above its paragraph.
var markdown = goldmark.New(
	// GFM, with column alignment as an attribute: a style attribute would be
	// refused by the page's own content security policy.
	goldmark.WithExtensions(
		extension.NewTable(extension.WithTableCellAlignMethod(extension.TableCellAlignAttribute)),
		extension.Strikethrough, extension.Linkify, extension.TaskList,
	),
	goldmark.WithRendererOptions(
		gmhtml.WithHardWraps(),
		renderer.WithNodeRenderers(util.Prioritized(fenceRenderer{}, 100)),
	),
)

func renderMarkdown(md string) template.HTML {
	var buf bytes.Buffer
	if err := markdown.Convert([]byte(md), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(md))
	}
	return template.HTML(buf.String())
}

// fenceRenderer draws hive: blocks and leaves other code blocks as code.
type fenceRenderer struct{}

func (fenceRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, func(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		n := node.(*ast.FencedCodeBlock)
		lang, code := string(n.Language(source)), fenceText(n, source)
		if kind, ok := strings.CutPrefix(lang, blockPrefix); ok {
			out, err := renderBlock(kind, code)
			if err == nil {
				w.WriteString(out)
				return ast.WalkSkipChildren, nil
			}
			// Writes are validated, so this is content from before a block
			// type changed. Show the data rather than nothing.
			fmt.Fprintf(w, `<p class="hb-error">This %s block could not be drawn: %s</p>`, html.EscapeString(lang), html.EscapeString(err.Error()))
		}
		w.WriteString("<pre><code")
		if lang != "" {
			fmt.Fprintf(w, ` class="language-%s"`, html.EscapeString(lang))
		}
		w.WriteString(">")
		w.WriteString(html.EscapeString(string(code)))
		w.WriteString("</code></pre>\n")
		return ast.WalkSkipChildren, nil
	})
}

func fenceText(n *ast.FencedCodeBlock, source []byte) []byte {
	var b bytes.Buffer
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(source))
	}
	return b.Bytes()
}

// validateBlocks checks every hive: block in a body, so an agent learns
// about a mistake when it posts rather than a person when they read.
func validateBlocks(body string) *Error {
	src := []byte(body)
	doc := markdown.Parser().Parse(text.NewReader(src))
	var found *Error
	count := 0
	ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		n, ok := node.(*ast.FencedCodeBlock)
		if !ok || !entering || found != nil {
			return ast.WalkContinue, nil
		}
		kind, ok := strings.CutPrefix(string(n.Language(src)), blockPrefix)
		if !ok {
			return ast.WalkContinue, nil
		}
		count++
		if _, err := renderBlock(kind, fenceText(n, src)); err != nil {
			found = errInvalid("hive:%s block (number %d in the body) is not valid: %s", kind, count, err.Error())
			found.Hint = "a block is a fenced code block holding one JSON object; " + blockHint(kind)
		}
		return ast.WalkContinue, nil
	})
	return found
}

// plainText is the readable text of a body, without Markdown syntax and
// without code or blocks, for previews. It stops soon after limit bytes.
func plainText(body string, limit int) string {
	src := []byte(body)
	doc := markdown.Parser().Parse(text.NewReader(src))
	var b strings.Builder
	ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			if node.Type() == ast.TypeBlock {
				b.WriteByte(' ')
			}
			return ast.WalkContinue, nil
		}
		switch n := node.(type) {
		case *ast.FencedCodeBlock, *ast.CodeBlock, *ast.HTMLBlock:
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			b.Write(n.Segment.Value(src))
			if n.SoftLineBreak() || n.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *ast.String:
			b.Write(n.Value)
		}
		if b.Len() > limit {
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

// ---- Block types ----

type blockType struct {
	Name    string
	Use     string // one line for the agent guide
	Example string // a minimal valid block
	render  func(raw []byte) (string, error)
}

var blockTypes = []blockType{
	{"metrics", "headline numbers side by side",
		`{"items":[{"label":"Tests passing","value":52,"delta":"+13","trend":"up","note":"since v0.1.1"}]}`, renderMetrics},
	{"chart", "bar, line or area chart of one or more series over shared x labels",
		`{"type":"bar","title":"p99 frame time","unit":"ms","x":["v0.1.0","v0.1.1"],"series":[{"name":"sample","data":[18.2,15.9]}]}`, renderChart},
	{"table", "a table people can sort by clicking a column",
		`{"columns":["Ticket",{"label":"Open days","align":"right"}],"rows":[["#26",3],["#19",11]]}`, renderTable},
	{"timeline", "dated events in order, each done, active, planned or blocked",
		`{"items":[{"date":"2026-08-18","title":"Windows port announced","text":"No date given","status":"done"}]}`, renderTimeline},
	{"board", "columns of cards, for tickets or work by state",
		`{"columns":[{"title":"In progress","items":[{"title":"#26 physics step","text":"picks the fastest mode","tag":"10.12"}]}]}`, renderBoard},
	{"progress", "how far along several things are",
		`{"items":[{"label":"Phase 11 networking","value":100,"note":"Verified"},{"label":"Phase 12","value":2,"max":3}]}`, renderProgress},
	{"callout", "one highlighted note: info, success, warning or danger",
		`{"tone":"warning","title":"Blocked on GPU quota","text":"Nothing can start until the quota request clears."}`, renderCallout},
	{"cards", "a grid of linked items, for a digest or a list of sources",
		`{"items":[{"title":"Qwen4-Max ships","text":"62% cheaper tasks on the vendor's own benchmark","url":"https://example.com/a","source":"InfoWorld","tag":"models"}]}`, renderCards},
}

func findBlock(kind string) *blockType {
	for i := range blockTypes {
		if blockTypes[i].Name == kind {
			return &blockTypes[i]
		}
	}
	return nil
}

func blockNames() string {
	names := make([]string, len(blockTypes))
	for i, b := range blockTypes {
		names[i] = b.Name
	}
	return strings.Join(names, ", ")
}

func blockHint(kind string) string {
	if b := findBlock(kind); b != nil {
		return "example: " + b.Example
	}
	return "block types: " + blockNames()
}

func renderBlock(kind string, raw []byte) (string, error) {
	b := findBlock(kind)
	if b == nil {
		return "", fmt.Errorf("there is no block type %q; the types are %s", kind, blockNames())
	}
	return b.render(raw)
}

// decodeBlock reads one JSON object strictly: an unknown field is an error,
// because a misspelt field would otherwise be silently ignored.
func decodeBlock(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var syn *json.SyntaxError
		var typ *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syn):
			return fmt.Errorf("the JSON does not parse (near byte %d)", syn.Offset)
		case errors.As(err, &typ):
			return fmt.Errorf("%q should be a %s", typ.Field, typ.Type)
		}
		return errors.New(strings.TrimPrefix(err.Error(), "json: "))
	}
	if dec.More() {
		return errors.New("only one JSON object is allowed in a block")
	}
	return nil
}

// flex is a JSON string, number or boolean, kept as the text to show.
type flex string

func (f *flex) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flex(s)
		return nil
	}
	if s := string(b); s == "null" {
		*f = ""
	} else if s == "true" || s == "false" || json.Valid(b) && (b[0] == '-' || b[0] >= '0' && b[0] <= '9') {
		*f = flex(s)
	} else {
		return errors.New("a value must be a string or a number")
	}
	return nil
}

func esc(s string) string { return html.EscapeString(strings.TrimSpace(s)) }

func oneOf(field, v string, allowed ...string) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return fmt.Errorf("%s must be one of: %s", field, strings.Join(allowed[1:], ", "))
}

func between(what string, n, min, max int) error {
	if n < min || n > max {
		return fmt.Errorf("%s: give between %d and %d", what, min, max)
	}
	return nil
}

// linkURL accepts only http and https links, and returns the escaped href.
func linkURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("url %q must be an http or https link", raw)
	}
	return html.EscapeString(u.String()), nil
}

func renderMetrics(raw []byte) (string, error) {
	var in struct {
		Items []struct {
			Label string `json:"label"`
			Value flex   `json:"value"`
			Delta flex   `json:"delta"`
			Trend string `json:"trend"`
			Note  string `json:"note"`
		} `json:"items"`
	}
	if err := decodeBlock(raw, &in); err != nil {
		return "", err
	}
	if err := between("items", len(in.Items), 1, 12); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(`<div class="hb hb-metrics">`)
	for _, it := range in.Items {
		if strings.TrimSpace(it.Label) == "" || it.Value == "" {
			return "", errors.New("every item needs a label and a value")
		}
		if err := oneOf("trend", it.Trend, "", "up", "down", "flat"); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, `<div class="hb-metric"><div class="hb-metric-label">%s</div><div class="hb-metric-value">%s</div>`, esc(it.Label), esc(string(it.Value)))
		if it.Delta != "" {
			fmt.Fprintf(&b, `<div class="hb-metric-delta %s">%s</div>`, it.Trend, esc(string(it.Delta)))
		}
		if it.Note != "" {
			fmt.Fprintf(&b, `<div class="hb-metric-note">%s</div>`, esc(it.Note))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString("</div>\n")
	return b.String(), nil
}

func renderCallout(raw []byte) (string, error) {
	var in struct {
		Tone  string `json:"tone"`
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	if err := decodeBlock(raw, &in); err != nil {
		return "", err
	}
	if in.Tone == "" {
		in.Tone = "info"
	}
	if err := oneOf("tone", in.Tone, "", "info", "success", "warning", "danger"); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Title+in.Text) == "" {
		return "", errors.New("give a title, a text or both")
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<div class="hb hb-callout %s">`, in.Tone)
	if in.Title != "" {
		fmt.Fprintf(&b, `<div class="hb-callout-title">%s</div>`, esc(in.Title))
	}
	if in.Text != "" {
		fmt.Fprintf(&b, `<div class="hb-callout-text">%s</div>`, esc(in.Text))
	}
	b.WriteString("</div>\n")
	return b.String(), nil
}

func renderProgress(raw []byte) (string, error) {
	var in struct {
		Items []struct {
			Label string   `json:"label"`
			Value float64  `json:"value"`
			Max   *float64 `json:"max"`
			Note  string   `json:"note"`
		} `json:"items"`
	}
	if err := decodeBlock(raw, &in); err != nil {
		return "", err
	}
	if err := between("items", len(in.Items), 1, 20); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(`<div class="hb hb-progress">`)
	for _, it := range in.Items {
		max := 100.0
		if it.Max != nil {
			max = *it.Max
		}
		if strings.TrimSpace(it.Label) == "" || max <= 0 || it.Value < 0 || it.Value > max {
			return "", errors.New("every item needs a label and a value between 0 and max (max defaults to 100)")
		}
		shown := fmt.Sprintf("%.0f%%", it.Value/max*100)
		if it.Max != nil {
			shown = num(it.Value) + " / " + num(max)
		}
		fmt.Fprintf(&b, `<div class="hb-progress-row"><div class="hb-progress-label">%s</div><progress value="%s" max="%s">%s</progress><div class="hb-progress-value">%s</div>`,
			esc(it.Label), num(it.Value), num(max), shown, shown)
		if it.Note != "" {
			fmt.Fprintf(&b, `<div class="hb-progress-note">%s</div>`, esc(it.Note))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString("</div>\n")
	return b.String(), nil
}

func renderTimeline(raw []byte) (string, error) {
	var in struct {
		Items []struct {
			Date   string `json:"date"`
			Title  string `json:"title"`
			Text   string `json:"text"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := decodeBlock(raw, &in); err != nil {
		return "", err
	}
	if err := between("items", len(in.Items), 1, 40); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(`<ol class="hb hb-timeline">`)
	for _, it := range in.Items {
		if strings.TrimSpace(it.Title) == "" {
			return "", errors.New("every item needs a title")
		}
		if err := oneOf("status", it.Status, "", "done", "active", "planned", "blocked"); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, `<li class="%s"><div class="hb-timeline-date">%s</div><div class="hb-timeline-title">%s`, it.Status, esc(it.Date), esc(it.Title))
		if it.Status != "" {
			fmt.Fprintf(&b, ` <span class="hb-pill %s">%s</span>`, it.Status, it.Status)
		}
		b.WriteString(`</div>`)
		if it.Text != "" {
			fmt.Fprintf(&b, `<div class="hb-timeline-text">%s</div>`, esc(it.Text))
		}
		b.WriteString(`</li>`)
	}
	b.WriteString("</ol>\n")
	return b.String(), nil
}

type cardItem struct {
	Title  string `json:"title"`
	Text   string `json:"text"`
	URL    string `json:"url"`
	Source string `json:"source"`
	Tag    string `json:"tag"`
}

func (c cardItem) html(b *strings.Builder, class string) error {
	if strings.TrimSpace(c.Title) == "" {
		return errors.New("every item needs a title")
	}
	href, err := linkURL(c.URL)
	if err != nil {
		return err
	}
	fmt.Fprintf(b, `<div class="%s">`, class)
	if c.Tag != "" {
		fmt.Fprintf(b, `<span class="hb-pill">%s</span>`, esc(c.Tag))
	}
	if href != "" {
		fmt.Fprintf(b, `<a class="hb-card-title" href="%s" rel="noopener noreferrer" target="_blank">%s</a>`, href, esc(c.Title))
	} else {
		fmt.Fprintf(b, `<div class="hb-card-title">%s</div>`, esc(c.Title))
	}
	if c.Text != "" {
		fmt.Fprintf(b, `<div class="hb-card-text">%s</div>`, esc(c.Text))
	}
	if c.Source != "" {
		fmt.Fprintf(b, `<div class="hb-card-source">%s</div>`, esc(c.Source))
	}
	b.WriteString(`</div>`)
	return nil
}

func renderCards(raw []byte) (string, error) {
	var in struct {
		Items []cardItem `json:"items"`
	}
	if err := decodeBlock(raw, &in); err != nil {
		return "", err
	}
	if err := between("items", len(in.Items), 1, 24); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(`<div class="hb hb-cards">`)
	for _, it := range in.Items {
		if err := it.html(&b, "hb-card"); err != nil {
			return "", err
		}
	}
	b.WriteString("</div>\n")
	return b.String(), nil
}

func renderBoard(raw []byte) (string, error) {
	var in struct {
		Columns []struct {
			Title string     `json:"title"`
			Items []cardItem `json:"items"`
		} `json:"columns"`
	}
	if err := decodeBlock(raw, &in); err != nil {
		return "", err
	}
	if err := between("columns", len(in.Columns), 1, 6); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(`<div class="hb hb-board">`)
	for _, col := range in.Columns {
		if strings.TrimSpace(col.Title) == "" || len(col.Items) > 30 {
			return "", errors.New("every column needs a title and at most 30 items")
		}
		fmt.Fprintf(&b, `<div class="hb-board-col"><div class="hb-board-head">%s <span class="hb-count">%d</span></div>`, esc(col.Title), len(col.Items))
		for _, it := range col.Items {
			if err := it.html(&b, "hb-card"); err != nil {
				return "", err
			}
		}
		b.WriteString(`</div>`)
	}
	b.WriteString("</div>\n")
	return b.String(), nil
}

func renderTable(raw []byte) (string, error) {
	var in struct {
		Caption string            `json:"caption"`
		Columns []json.RawMessage `json:"columns"`
		Rows    [][]flex          `json:"rows"`
	}
	if err := decodeBlock(raw, &in); err != nil {
		return "", err
	}
	if err := between("columns", len(in.Columns), 1, 12); err != nil {
		return "", err
	}
	if err := between("rows", len(in.Rows), 1, 300); err != nil {
		return "", err
	}
	type column struct {
		Label string `json:"label"`
		Align string `json:"align"`
	}
	cols := make([]column, len(in.Columns))
	for i, c := range in.Columns {
		if json.Unmarshal(c, &cols[i].Label) != nil {
			if err := decodeBlock(c, &cols[i]); err != nil {
				return "", fmt.Errorf("a column is a string or {\"label\", \"align\"}: %w", err)
			}
		}
		if err := oneOf("align", cols[i].Align, "", "left", "right", "center"); err != nil {
			return "", err
		}
	}
	var b strings.Builder
	b.WriteString(`<div class="hb hb-table table-wrap"><table data-sortable>`)
	if in.Caption != "" {
		fmt.Fprintf(&b, `<caption>%s</caption>`, esc(in.Caption))
	}
	b.WriteString(`<thead><tr>`)
	for _, c := range cols {
		fmt.Fprintf(&b, `<th class="%s" scope="col">%s</th>`, c.Align, esc(c.Label))
	}
	b.WriteString(`</tr></thead><tbody>`)
	for n, row := range in.Rows {
		if len(row) != len(cols) {
			return "", fmt.Errorf("row %d has %d cells but there are %d columns", n+1, len(row), len(cols))
		}
		b.WriteString(`<tr>`)
		for i, cell := range row {
			fmt.Fprintf(&b, `<td class="%s">%s</td>`, cols[i].Align, esc(string(cell)))
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString("</tbody></table></div>\n")
	return b.String(), nil
}

// ---- Charts ----

// num formats a number the way a person would write it.
func num(v float64) string {
	return strconv.FormatFloat(math.Round(v*1000)/1000, 'f', -1, 64)
}

// niceCeil rounds up to 1, 2, 2.5 or 5 times a power of ten.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(v)))
	switch f := v / exp; {
	case f <= 1:
		return exp
	case f <= 2:
		return 2 * exp
	case f <= 2.5:
		return 2.5 * exp
	case f <= 5:
		return 5 * exp
	}
	return 10 * exp
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func renderChart(raw []byte) (string, error) {
	var in struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Unit   string `json:"unit"`
		X      []flex `json:"x"`
		Series []struct {
			Name string     `json:"name"`
			Data []*float64 `json:"data"`
		} `json:"series"`
	}
	if err := decodeBlock(raw, &in); err != nil {
		return "", err
	}
	if in.Type == "" {
		in.Type = "bar"
	}
	if err := oneOf("type", in.Type, "", "bar", "line", "area"); err != nil {
		return "", err
	}
	if err := between("x", len(in.X), 1, 60); err != nil {
		return "", err
	}
	if err := between("series", len(in.Series), 1, 6); err != nil {
		return "", err
	}
	lo, hi := 0.0, 0.0
	for _, s := range in.Series {
		if len(s.Data) != len(in.X) {
			return "", fmt.Errorf("series %q has %d values but x has %d labels", s.Name, len(s.Data), len(in.X))
		}
		for _, v := range s.Data {
			if v == nil {
				continue
			}
			if math.IsNaN(*v) || math.IsInf(*v, 0) {
				return "", errors.New("values must be finite numbers, or null for a gap")
			}
			lo, hi = math.Min(lo, *v), math.Max(hi, *v)
		}
	}
	hi = niceCeil(hi)
	if lo < 0 {
		lo = -niceCeil(-lo)
	}
	if hi == lo {
		hi = lo + 1
	}

	const width, height, left, right, top, bottom = 720.0, 300.0, 56.0, 12.0, 14.0, 40.0
	plotW, plotH := width-left-right, height-top-bottom
	y := func(v float64) float64 { return top + plotH*(1-(v-lo)/(hi-lo)) }
	band := plotW / float64(len(in.X))
	cx := func(i int) float64 { return left + band*(float64(i)+0.5) }
	unit := ""
	if in.Unit != "" {
		unit = " " + in.Unit
	}

	var b strings.Builder
	b.WriteString(`<figure class="hb hb-chart">`)
	if in.Title != "" || in.Unit != "" {
		fmt.Fprintf(&b, `<figcaption>%s<span class="hb-unit">%s</span></figcaption>`, esc(in.Title), esc(in.Unit))
	}
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, width, height, esc(in.Title+" "+in.Type+" chart"))
	for i := 0; i <= 4; i++ {
		v := lo + (hi-lo)*float64(i)/4
		fmt.Fprintf(&b, `<line class="grid" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/><text class="tick" x="%.1f" y="%.1f" text-anchor="end">%s</text>`,
			left, width-right, y(v), y(v), left-6, y(v)+4, esc(num(v)))
	}
	every := (len(in.X) + 11) / 12 // at most a dozen x labels
	for i, label := range in.X {
		if i%every == 0 {
			fmt.Fprintf(&b, `<text class="tick" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, cx(i), height-bottom+16, esc(clip(string(label), 14)))
		}
	}
	fmt.Fprintf(&b, `<line class="axis" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, left, width-right, y(0), y(0))

	for si, s := range in.Series {
		name := s.Name
		if name == "" {
			name = fmt.Sprintf("series %d", si+1)
		}
		tip := func(i int, v float64) string {
			return fmt.Sprintf(`<title>%s · %s: %s%s</title>`, esc(name), esc(string(in.X[i])), num(v), esc(unit))
		}
		if in.Type == "bar" {
			group := band * 0.72
			w := group / float64(len(in.Series))
			for i, v := range s.Data {
				if v == nil {
					continue
				}
				x := cx(i) - group/2 + w*float64(si)
				fmt.Fprintf(&b, `<rect class="s%d" x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2">%s</rect>`,
					si, x, math.Min(y(*v), y(0)), math.Max(w-1, 1), math.Abs(y(*v)-y(0)), tip(i, *v))
			}
			continue
		}
		// A line or area breaks at a null, so a gap is drawn as a gap.
		var runs [][]int
		var run []int
		for i, v := range s.Data {
			if v == nil {
				if run != nil {
					runs, run = append(runs, run), nil
				}
				continue
			}
			run = append(run, i)
		}
		if run != nil {
			runs = append(runs, run)
		}
		for _, run := range runs {
			var pts strings.Builder
			for _, i := range run {
				fmt.Fprintf(&pts, "%.1f,%.1f ", cx(i), y(*s.Data[i]))
			}
			if in.Type == "area" {
				fmt.Fprintf(&b, `<polygon class="area s%d" points="%.1f,%.1f %s%.1f,%.1f"/>`, si, cx(run[0]), y(0), pts.String(), cx(run[len(run)-1]), y(0))
			}
			fmt.Fprintf(&b, `<polyline class="line s%d" points="%s"/>`, si, strings.TrimSpace(pts.String()))
		}
		for i, v := range s.Data {
			if v != nil {
				fmt.Fprintf(&b, `<circle class="dot s%d" cx="%.1f" cy="%.1f" r="3.5">%s</circle>`, si, cx(i), y(*v), tip(i, *v))
			}
		}
	}
	b.WriteString(`</svg>`)
	if len(in.Series) > 1 || in.Series[0].Name != "" {
		b.WriteString(`<div class="hb-legend">`)
		for si, s := range in.Series {
			fmt.Fprintf(&b, `<span><svg viewBox="0 0 10 10" class="hb-swatch"><rect class="s%d" width="10" height="10" rx="2"/></svg>%s</span>`, si, esc(s.Name))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString("</figure>\n")
	return b.String(), nil
}

// blockGuide is the block reference for the agent guide.
func blockGuide() string {
	var b strings.Builder
	sorted := append([]blockType(nil), blockTypes...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, t := range sorted {
		fmt.Fprintf(&b, "- hive:%s: %s\n    %s\n", t.Name, t.Use, t.Example)
	}
	return b.String()
}
