// Package render turns note content into something readable: markdown to
// styled HTML or terminal output, other file types to highlighted code.
package render

import (
	"bytes"
	"html/template"
	"io"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/glamour"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"

	"github.com/EmreErdogan/np/internal/store"
)

// md renders GitHub-flavoured markdown. Raw HTML in notes is escaped, not
// rendered, so a note cannot inject script into the page.
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM, extension.Typographer),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

func lexerFor(name string) chroma.Lexer {
	l := lexers.Match(store.FileName(name))
	if l == nil {
		l = lexers.Fallback
	}
	return chroma.Coalesce(l)
}

// HTML renders a note for the web UI. Markdown notes become a document;
// anything else becomes a highlighted code block using chroma CSS classes
// (see CSS).
func HTML(name string, src []byte) template.HTML {
	if store.Ext(name) == "" {
		var buf bytes.Buffer
		if err := md.Convert(src, &buf); err == nil {
			return template.HTML(buf.String())
		}
		return template.HTML("<pre>" + template.HTMLEscapeString(string(src)) + "</pre>")
	}
	it, err := lexerFor(name).Tokenise(nil, string(src))
	if err != nil {
		return template.HTML("<pre>" + template.HTMLEscapeString(string(src)) + "</pre>")
	}
	var buf bytes.Buffer
	f := chromahtml.New(chromahtml.WithClasses(true), chromahtml.WithLineNumbers(false))
	if err := f.Format(&buf, styles.Get("github"), it); err != nil {
		return template.HTML("<pre>" + template.HTMLEscapeString(string(src)) + "</pre>")
	}
	return template.HTML(buf.String())
}

// CSS returns chroma class styles for light and dark schemes.
func CSS() string {
	var light, dark bytes.Buffer
	f := chromahtml.New(chromahtml.WithClasses(true))
	f.WriteCSS(&light, styles.Get("github"))
	f.WriteCSS(&dark, styles.Get("github-dark"))
	return light.String() + "\n@media(prefers-color-scheme:dark){" + strings.ReplaceAll(dark.String(), "\n", "\n") + "}"
}

// Terminal writes a note to w with ANSI styling: glamour for markdown,
// chroma for code. dark selects the colour scheme.
func Terminal(w io.Writer, name string, src []byte, width int, dark bool) error {
	if store.Ext(name) == "" {
		style := glamour.WithStandardStyle("light")
		if dark {
			style = glamour.WithStandardStyle("dark")
		}
		r, err := glamour.NewTermRenderer(style, glamour.WithWordWrap(width))
		if err != nil {
			return err
		}
		out, err := r.Render(string(src))
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, out)
		return err
	}
	it, err := lexerFor(name).Tokenise(nil, string(src))
	if err != nil {
		return err
	}
	theme := "github"
	if dark {
		theme = "github-dark"
	}
	return formatters.Get("terminal256").Format(w, styles.Get(theme), it)
}
