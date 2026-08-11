package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mdText joins rendered lines back into flat text, the shape the assertions compare.
// A table histLine is expanded through layoutTable since its cells live in structure,
// not in l.text.
func mdText(lines []histLine) string {
	var b strings.Builder
	for _, l := range lines {
		if l.table != nil {
			for _, row := range layoutTable(l.table, defaultWidth) {
				b.WriteString(row)
				b.WriteString("\n")
			}
		} else {
			b.WriteString(l.text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func TestRenderMarkdown(t *testing.T) {
	t.Parallel()

	plain := NewTheme(ColorNone)

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty", "   \n ", ""},
		{"paragraph", "hello world", "hello world\n"},
		{"soft_break_reflows", "one\ntwo", "one two\n"},
		{"hard_break_kept", "one  \ntwo", "one\ntwo\n"},
		{"heading_keeps_level", "### Deep", "### Deep\n"},
		{"blocks_separated", "one\n\ntwo", "one\n\ntwo\n"},
		{"bullet_list", "- a\n- b", "• a\n• b\n"},
		{"ordered_list_start", "3. a\n4. b", "3. a\n4. b\n"},
		{"nested_list_indent", "- a\n  - b", "• a\n  • b\n"},
		{"task_list", "- [x] done\n- [ ] open", "• [x] done\n• [ ] open\n"},
		{"blockquote", "> quoted", "▏ quoted\n"},
		{"fenced_code", "```go\nx := 1\n```", "  go\n  x := 1\n"},
		{"indented_code", "    x := 1", "  x := 1\n"},
		{"inline_styles_stripped", "a **b** *c* `d`", "a b c d\n"},
		{"link_shows_url", "[text](https://x.dev)", "text (https://x.dev)\n"},
		{"link_matching_url_once", "[https://x.dev](https://x.dev)", "https://x.dev\n"},
		{"autolink", "<https://x.dev>", "https://x.dev\n"},
		{"strikethrough", "~~gone~~", "gone\n"},
		{"image", "![alt](x.png)", "[image: alt]\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, mdText(renderMarkdown(plain, 40, tc.input)))
		})
	}

	t.Run("thematic_break_uses_width", func(t *testing.T) {
		rule := strings.Repeat(ruleChar, 12) + "\n"
		assert.Equal(t, rule, mdText(renderMarkdown(plain, 12, "---")))
		assert.Equal(t, rule, mdText(renderMarkdown(plain, 12, "----")))
		assert.Equal(t, rule, mdText(renderMarkdown(plain, 12, "***")))
	})
	t.Run("table_rendered", func(t *testing.T) {
		out := mdText(renderMarkdown(plain, 60, "| A | B |\n|---|---|\n| 1 | 2 |"))
		assert.Contains(t, out, "│ A │ B │")
		assert.Contains(t, out, "│ 1 │ 2 │")
	})
}

// TestRenderMarkdownFlow guards the classification the renderers act on, which
// used to be inferred from the line text.
func TestRenderMarkdownFlow(t *testing.T) {
	t.Parallel()

	plain := NewTheme(ColorNone)

	tests := []struct {
		name     string
		input    string
		expected lineFlow
	}{
		{"paragraph_reflows", "just some prose", flowReflow},
		{"heading_reflows", "# Title", flowReflow},
		{"prose_at_reflows", "@channel please review", flowReflow},
		{"prose_plus_reflows", "+1 from me", flowReflow},
		{"prose_box_rune_reflows", "─ is a box drawing rune", flowReflow},
		{"list_wraps", "- a\n- b", flowWrap},
		{"code_wraps", "```go\nx := 1\n```", flowWrap},
		{"blockquote_wraps", "> quoted", flowWrap},
		{"rule_clips", "---", flowClip},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lines := renderMarkdown(plain, 40, tc.input)
			require.NotEmpty(t, lines)
			for _, l := range lines {
				if l.table != nil { // tables are structured and re-laid out per width
					continue
				}
				assert.Equal(t, tc.expected, l.flow)
			}
		})
	}

	t.Run("blocks_keep_own_flow", func(t *testing.T) {
		lines := renderMarkdown(plain, 40, "prose\n\n---")
		require.Len(t, lines, 3)
		assert.Equal(t, flowReflow, lines[0].flow)
		assert.Empty(t, lines[1].text) // separator
		assert.Equal(t, flowClip, lines[2].flow)
	})
}

// TestRenderMarkdownTable guards the structured table path: separators between rows,
// long cells wrapped within their column, and re-layout at a narrower width.
func TestRenderMarkdownTable(t *testing.T) {
	t.Parallel()

	plain := NewTheme(ColorNone)
	const src = "| ID | Name |\n|---|---|\n| 1 | alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma tau upsilon phi chi psi omega |"

	t.Run("one_structured_line_with_separators", func(t *testing.T) {
		lines := renderMarkdown(plain, 60, src)
		require.Len(t, lines, 1)
		require.NotNil(t, lines[0].table)

		shape := layoutTable(lines[0].table, 80)
		assert.True(t, strings.HasPrefix(shape[0], "┌────┬"), "top border: %q", shape[0])
		assert.Contains(t, shape[1], "│ ID │ Name")
		// the header and data rows are separated by a mid line
		assert.True(t, strings.HasPrefix(shape[2], "├────┼"), "mid border: %q", shape[2])
	})
	t.Run("long_cell_wraps_within_the_column", func(t *testing.T) {
		lines := renderMarkdown(plain, 60, src)
		rows := layoutTable(lines[0].table, 40)
		// every physical line keeps its left border and nothing is lost
		for _, row := range rows {
			assert.True(t, strings.HasPrefix(row, "┌") || strings.HasPrefix(row, "├") ||
				strings.HasPrefix(row, "└") || strings.HasPrefix(row, "│"), "row: %q", row)
		}
	})
	t.Run("reflows_to_a_narrower_width", func(t *testing.T) {
		lines := renderMarkdown(plain, 60, src)
		wide := layoutTable(lines[0].table, 80)
		narrow := layoutTable(lines[0].table, 30)

		require.Greater(t, len(narrow), len(wide), "a narrower terminal should wrap more")
	})
}

func TestRenderMarkdownStyled(t *testing.T) {
	t.Parallel()

	th := NewTheme(Color256)

	t.Run("bold_wrapped", func(t *testing.T) {
		assert.Equal(t, "a \x1b[1mb\x1b[0m\n", mdText(renderMarkdown(th, 40, "a **b**")))
	})
	t.Run("nested_style_restores_parent", func(t *testing.T) {
		out := mdText(renderMarkdown(th, 40, "**bold `code` more**"))
		// the code span reset must be followed by a reopen of the bold style
		assert.Contains(t, out, sgrReset+th.Bold.Open()+" more")
	})
	t.Run("heading_styled", func(t *testing.T) {
		assert.Equal(t, th.Dim.Wrap("# ")+th.Heading.Wrap("Title")+"\n", mdText(renderMarkdown(th, 40, "# Title")))
	})
}

// TestRenderMarkdownDocument guards the full element set the demo exercises.
func TestRenderMarkdownDocument(t *testing.T) {
	t.Parallel()

	th := NewTheme(Color256)
	src := strings.Join([]string{
		"## Heading two",
		"",
		"### Heading three",
		"",
		"prose with **bold**, *italic*, `code` and ~~struck~~ text",
		"",
		"- bullet one",
		"- bullet two",
		"",
		"```go",
		"func f() int { return 1 }",
		"```",
		"",
		"| A | B |",
		"|---|--:|",
		"| 1 | 2 |",
		"",
		"> quoted line",
		"",
		"----",
		"",
		"1. first",
		"2. second",
		"",
		"see [the notes](https://ajent.dev/x)",
		"",
	}, "\n")
	out := mdText(renderMarkdown(th, 60, src))

	elements := map[string]string{
		"heading":        th.Heading.Wrap("Heading two"),
		"sub heading":    th.Heading.Wrap("Heading three"),
		"bold":           th.Bold.Wrap("bold"),
		"italic":         th.Italic.Wrap("italic"),
		"inline code":    th.Code.Wrap("code"),
		"strikethrough":  th.Strike.Wrap("struck"),
		"code block":     codeIndent + th.Code.Wrap("func f() int { return 1 }"),
		"code language":  th.Dim.Wrap(codeIndent + "go"),
		"bullet":         th.Accent.Wrap(bulletMarker) + "bullet one",
		"ordered marker": th.Accent.Wrap("1. ") + "first",
		"blockquote":     th.Dim.Wrap(quotePrefix) + th.Quote.Wrap("quoted line"),
		"rule":           th.Dim.Wrap(strings.Repeat(ruleChar, 60)),
		"link":           th.Link.Wrap("the notes"),
		"link url":       th.Dim.Wrap(" (https://ajent.dev/x)"),
		"table border":   "┌",
		"table cell":     "│ A │",
	}
	for name, want := range elements {
		assert.Contains(t, out, want, "missing rendered %s", name)
	}
}

func TestSplitCompleteBlocks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        string
		expectedDone string
		expectedRest string
	}{
		{"no_boundary_yet", "hello wor", "", "hello wor"},
		{"single_line_no_blank", "hello\n", "", "hello\n"},
		{"complete_paragraph", "hello\n\nnext", "hello\n\n", "next"},
		{"multiple_blocks", "a\n\nb\n\nc", "a\n\nb\n\n", "c"},
		{"open_fence_held", "```go\nx := 1\n\ny := 2\n", "", "```go\nx := 1\n\ny := 2\n"},
		{"closed_fence_released", "```go\nx := 1\n```\n", "```go\nx := 1\n```\n", ""},
		{"tilde_fence", "~~~\nx\n~~~\n", "~~~\nx\n~~~\n", ""},
		{"fence_then_partial", "```\nx\n```\nnext par", "```\nx\n```\n", "next par"},
		{"partial_line_held", "a\n\nb", "a\n\n", "b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			done, rest := splitCompleteBlocks(tc.input)
			assert.Equal(t, tc.expectedDone, done)
			assert.Equal(t, tc.expectedRest, rest)
		})
	}
}

func TestIndentLines(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "> a\n  b", indentLines("a\nb", "> ", "  "))
	assert.Equal(t, "> a", indentLines("a", "> ", "  "))
	assert.Equal(t, "| a\n| b", prefixLines("a\nb", "| "))
}

func TestFenceMarker(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "```", fenceMarker("```go"))
	assert.Equal(t, "~~~", fenceMarker("~~~"))
	assert.Empty(t, fenceMarker("nope"))
}
