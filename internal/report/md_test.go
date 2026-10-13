package report

import "testing"

func TestMarkdownToHTML(t *testing.T) {
	got := MarkdownToHTML("## Overview\nShe *said* it was **fine** [M-0001, M-0002] and [M-9999].\n- 2*3*4 & <b>\n* item *x*", map[string]bool{"M-0001": true, "M-0002": true})
	want := `<h3>Overview</h3>
<p>She <em>said</em> it was <strong>fine</strong> [<a class="ref" href="#M-0001">M-0001</a>, <a class="ref" href="#M-0002">M-0002</a>] and [M-9999].</p>
<ul>
<li>2*3*4 &amp; &lt;b&gt;</li>
<li>item <em>x</em></li>
</ul>`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
