package lint

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

var (
	mdLink  = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	fence   = regexp.MustCompile("(?ms)^\\s*```.*?^\\s*```")
	inlineC = regexp.MustCompile("`[^`\n]*`")
	heading = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*#*\s*$`)
)

// markdownFiles lists every Markdown file in the repository, relative to
// its root, outside hidden and build directories.
func markdownFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() && path != root && (strings.HasPrefix(name, ".") || name == "bin" || name == "bin-linux" || name == "node_modules") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(name, ".md") {
			rel, _ := filepath.Rel(root, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// prose is a Markdown file without its code, where links and headings are
// not links and headings.
func prose(text string) string {
	return inlineC.ReplaceAllString(fence.ReplaceAllString(text, ""), "")
}

// slug is the anchor GitHub gives a heading: lower case, punctuation
// removed, spaces turned into hyphens; letters of any script are kept.
func slug(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// anchors lists a file's heading anchors, numbering repeated ones as
// GitHub does (x, x-1, x-2, ...).
func anchors(text string) map[string]bool {
	out := map[string]bool{}
	seen := map[string]int{}
	for _, m := range heading.FindAllStringSubmatch(prose(text), -1) {
		s := slug(m[1])
		if n := seen[s]; n > 0 {
			out[fmt.Sprintf("%s-%d", s, n)] = true
		} else {
			out[s] = true
		}
		seen[s]++
	}
	return out
}

// TestDocLinks checks that every relative link in the repository's
// Markdown points to a file that exists, and every #anchor to a heading.
// Links to the web are not checked here.
func TestDocLinks(t *testing.T) {
	root := filepath.Join("..", "..")
	files := markdownFiles(t, root)
	if len(files) < 10 {
		t.Fatalf("found only %d Markdown files under %s", len(files), root)
	}
	cache := map[string]map[string]bool{}
	anchorsOf := func(rel string) map[string]bool {
		if a, ok := cache[rel]; ok {
			return a
		}
		data, _ := os.ReadFile(filepath.Join(root, rel))
		cache[rel] = anchors(string(data))
		return cache[rel]
	}
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range mdLink.FindAllStringSubmatch(prose(string(data)), -1) {
			link := m[1]
			if strings.Contains(link, "://") || strings.HasPrefix(link, "mailto:") {
				continue
			}
			path, frag, _ := strings.Cut(link, "#")
			target := rel
			if path != "" {
				target = filepath.ToSlash(filepath.Join(filepath.Dir(rel), path))
				if _, err := os.Stat(filepath.Join(root, target)); err != nil {
					t.Errorf("%s: link to %s: no such file", rel, link)
					continue
				}
			}
			if frag != "" && strings.HasSuffix(target, ".md") && !anchorsOf(target)[frag] {
				t.Errorf("%s: link to %s: no heading with that anchor in %s", rel, link, target)
			}
		}
	}
}

// TestDocTranslations checks that the README and every document in docs/
// exist in English and in Chinese, each pointing to the other at the top.
func TestDocTranslations(t *testing.T) {
	root := filepath.Join("..", "..")
	pairs := 0
	for _, rel := range markdownFiles(t, root) {
		dir, name := filepath.Split(rel)
		if !(rel == "README.md" || rel == "README.zh-CN.md" || dir == "docs/") {
			continue
		}
		var en, zh string
		if base, ok := strings.CutSuffix(name, ".zh-CN.md"); ok {
			en, zh = base+".md", name
		} else {
			en, zh = name, strings.TrimSuffix(name, ".md")+".zh-CN.md"
		}
		want := map[string]string{
			en: "English | [简体中文](" + zh + ")",
			zh: "[English](" + en + ") | 简体中文",
		}
		for file, first := range want {
			data, err := os.ReadFile(filepath.Join(root, dir, file))
			if err != nil {
				t.Errorf("%s has no counterpart %s%s", rel, dir, file)
				continue
			}
			if line, _, _ := strings.Cut(string(data), "\n"); line != first {
				t.Errorf("%s%s: first line %q, want %q", dir, file, line, first)
			}
		}
		pairs++
	}
	if pairs < 2 {
		t.Fatalf("found only %d translated documents", pairs)
	}
}
