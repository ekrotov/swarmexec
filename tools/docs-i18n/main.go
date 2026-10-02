// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// docs-i18n keeps site/docs.<lang>.html in step with site/docs.html.
//
//	go run ./tools/docs-i18n            translate the sections whose English changed (Claude API)
//	go run ./tools/docs-i18n -dry-run   list them, call nothing
//	go run ./tools/docs-i18n -accept    record the current translations as up to date
//	                                    (after translating by hand)
//	go run ./tools/docs-i18n -seed      first run: record every section as translated
//
// Only stale sections are sent, each with the English it was translated from,
// the new English and the current translation, so the model changes what
// changed and leaves the rest word for word. Every answer is checked before it
// is written: same tables, command blocks, flags and keys as the English, and
// in the page head the per-language lines (lang, canonical, og:url,
// og:locale, hreflang) exactly as they were. A section that fails is not
// written; the run reports it and the CI check keeps failing until it is done.
//
// The API key comes from ANTHROPIC_API_KEY (or any credential the SDK finds).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"swarmexec/internal/docsi18n"
)

var languages = map[string]string{"de": "German", "es": "Spanish", "fr": "French", "pl": "Polish"}

func main() {
	site := flag.String("site", "site", "directory holding docs.html and its translations")
	only := flag.String("langs", "de,es,fr,pl", "languages to process")
	dry := flag.Bool("dry-run", false, "list stale sections, call nothing")
	accept := flag.Bool("accept", false, "record the current translations as up to date")
	seed := flag.Bool("seed", false, "alias of -accept for the first run")
	flag.Parse()

	if err := run(*site, strings.Split(*only, ","), *dry, *accept || *seed, func() translator { return newClaude() }); err != nil {
		fmt.Fprintln(os.Stderr, "docs-i18n:", err)
		os.Exit(1)
	}
}

func run(site string, langs []string, dry, accept bool, newTranslator func() translator) error {
	enPage, err := os.ReadFile(filepath.Join(site, "docs.html"))
	if err != nil {
		return err
	}
	en := docsi18n.Split(string(enPage))

	srcPath := filepath.Join(site, "i18n", "sources.json")
	sources, err := docsi18n.LoadSources(srcPath)
	if err != nil {
		return err
	}
	var allMems []docsi18n.Memory
	defer func() {
		if dry {
			return
		}
		// Every language's memory, not only the ones processed, so a partial
		// run does not prune a source another language still needs.
		for l := range languages {
			if m, err := docsi18n.LoadMemory(filepath.Join(site, "i18n", l+".json")); err == nil {
				allMems = append(allMems, m)
			}
		}
		sources.Prune(allMems...)
		if err := docsi18n.SaveJSON(srcPath, sources); err != nil {
			fmt.Fprintln(os.Stderr, "docs-i18n: save sources:", err)
		}
	}()

	var tr translator
	failed := 0
	for _, lang := range langs {
		name, ok := languages[lang]
		if !ok {
			return fmt.Errorf("unknown language %q", lang)
		}
		pagePath := filepath.Join(site, "docs."+lang+".html")
		memPath := filepath.Join(site, "i18n", lang+".json")
		page, err := os.ReadFile(pagePath)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		mem, err := docsi18n.LoadMemory(memPath)
		if err != nil {
			return err
		}
		current := map[string]string{}
		for _, s := range docsi18n.Split(string(page)) {
			current[s.ID] = s.Body
		}

		if accept {
			if probs := docsi18n.Stale(en, docsi18n.Split(string(page)), docsi18n.Memory{}); hasStructural(probs) {
				return fmt.Errorf("%s: cannot accept a page whose sections do not match the English: %v", lang, probs)
			}
			for _, s := range en {
				h := docsi18n.Hash(s.Body)
				mem[s.ID], sources[h] = h, s.Body
			}
			if err := os.MkdirAll(filepath.Dir(memPath), 0o755); err != nil {
				return err
			}
			if err := docsi18n.SaveJSON(memPath, mem); err != nil {
				return err
			}
			fmt.Printf("%s: %d sections recorded as up to date\n", lang, len(en))
			continue
		}

		var out []docsi18n.Section
		changed := 0
		for _, s := range en {
			prev, have := current[s.ID]
			if h, ok := mem[s.ID]; have && ok && h == docsi18n.Hash(s.Body) {
				out = append(out, docsi18n.Section{ID: s.ID, Body: prev})
				continue
			}
			fmt.Printf("%s: #%s is stale\n", lang, s.ID)
			if dry {
				out = append(out, docsi18n.Section{ID: s.ID, Body: prev})
				continue
			}
			if tr == nil {
				tr = newTranslator()
			}
			body, err := translateChecked(tr, name, s, sources[mem[s.ID]], prev)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: #%s NOT updated: %v\n", lang, s.ID, err)
				failed++
				if have {
					out = append(out, docsi18n.Section{ID: s.ID, Body: prev})
				}
				continue
			}
			out = append(out, docsi18n.Section{ID: s.ID, Body: body})
			h := docsi18n.Hash(s.Body)
			mem[s.ID], sources[h] = h, s.Body
			changed++
		}
		if dry || changed == 0 {
			continue
		}
		if err := os.WriteFile(pagePath, []byte(docsi18n.Join(out)), 0o644); err != nil {
			return err
		}
		if err := docsi18n.SaveJSON(memPath, mem); err != nil {
			return err
		}
		fmt.Printf("%s: %d sections updated\n", lang, changed)
	}
	if failed > 0 {
		return fmt.Errorf("%d sections could not be translated — see above", failed)
	}
	return nil
}

func hasStructural(probs []string) bool {
	for _, p := range probs {
		if !strings.Contains(p, "English changed") {
			return true
		}
	}
	return false
}

// translator turns one English section into the target language.
type translator interface {
	translate(lang string, sec docsi18n.Section, oldEnglish, oldTranslation string) (string, error)
}

// translateChecked translates and refuses an answer whose structure differs
// from the English, or that changed the head's per-language lines.
func translateChecked(t translator, lang string, sec docsi18n.Section, oldEnglish, oldTranslation string) (string, error) {
	body, err := t.translate(lang, sec, oldEnglish, oldTranslation)
	if err != nil {
		return "", err
	}
	if probs := docsi18n.Mismatch(docsi18n.ShapeOf(sec.Body), docsi18n.ShapeOf(body)); len(probs) > 0 {
		return "", fmt.Errorf("translation changed the structure: %s", strings.Join(probs, "; "))
	}
	if sec.ID == docsi18n.HeadID && oldTranslation != "" {
		if want, got := docsi18n.HeadFacts(oldTranslation), docsi18n.HeadFacts(body); strings.Join(want, "\n") != strings.Join(got, "\n") {
			return "", errors.New("translation changed the head's per-language lines (lang, canonical, og:url, og:locale, hreflang)")
		}
	}
	if sec.ID != docsi18n.HeadID && !strings.HasPrefix(body, sec.Body[:strings.Index(sec.Body, ">")+1]) {
		return "", errors.New("translation does not start with the section's heading tag")
	}
	return body, nil
}

const systemPrompt = `You translate the documentation page of swarmexec, a command-line tool and terminal UI for Docker Swarm, from English into %[1]s. You receive one section of the page as an HTML fragment.

Rules:
- Return only the translated HTML fragment: no explanation, no Markdown fences.
- Keep every tag, attribute, id, class, href and entity exactly. Translate the prose between the tags.
- Never translate what a user types or reads verbatim from the tool: the contents of <code>, <kbd>, <pre> and of class="cmd" / class="prompt" blocks, commands, flags, file names, configuration keys, product names. Comments inside command blocks (class="o") may be translated.
- Keep the voice, terms and form of address of the current %[1]s translation when one is given.
- When the English it was translated from is given, change only what changed between that and the new English. Every sentence whose English did not change stays exactly as it is in the current translation.
- In the page head, the lines carrying lang=, rel="canonical", og:url, og:locale and hreflang are facts about the %[1]s page: copy them unchanged from the current translation.`

type claude struct{ c anthropic.Client }

func newClaude() *claude { return &claude{c: anthropic.NewClient()} }

func (cl *claude) translate(lang string, sec docsi18n.Section, oldEnglish, oldTranslation string) (string, error) {
	var b strings.Builder
	if oldTranslation != "" && oldEnglish != "" {
		fmt.Fprintf(&b, "The English this section was translated from:\n<old_english>\n%s</old_english>\n\n", oldEnglish)
	}
	if oldTranslation != "" {
		fmt.Fprintf(&b, "The current %s translation:\n<current_translation>\n%s</current_translation>\n\n", lang, oldTranslation)
	}
	fmt.Fprintf(&b, "The new English to translate:\n<new_english>\n%s</new_english>\n\nReturn the %s HTML for <new_english>.", sec.Body, lang)

	stream := cl.c.Messages.NewStreaming(context.Background(), anthropic.MessageNewParams{
		Model:     "claude-opus-5-5",
		MaxTokens: 64000,
		System:    []anthropic.TextBlockParam{{Text: fmt.Sprintf(systemPrompt, lang)}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(b.String()))},
	},
		// A policy decline is re-served by Anthropic's recommended fallback
		// model in the same call instead of failing the section.
		option.WithHeaderAdd("anthropic-beta", "server-side-fallback-2026-07-01"),
		option.WithJSONSet("fallbacks", "default"),
	)
	msg := anthropic.Message{}
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return "", err
		}
	}
	if err := stream.Err(); err != nil {
		return "", err
	}
	switch msg.StopReason {
	case anthropic.StopReasonRefusal:
		return "", fmt.Errorf("the model declined (%s)", msg.StopDetails.Category)
	case anthropic.StopReasonMaxTokens:
		return "", errors.New("the answer was cut off at max_tokens")
	}
	var out strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.WriteString(t.Text)
		}
	}
	return strings.TrimSpace(out.String()) + trailingWhitespace(sec.Body), nil
}

// trailingWhitespace keeps the section's own line ending, which the model
// tends to drop, so Join reproduces the page's layout.
func trailingWhitespace(s string) string {
	return s[len(strings.TrimRight(s, " \t\n")):]
}
