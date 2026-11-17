# TUI keymap charter

How keys are chosen in `swarmexec ui`, so a new tab or overlay fits the ones
that exist instead of drifting away from them. This is the developer-facing
rulebook; the operator-facing reference is the `?` overlay (generated from the
live keymap) and [site/docs/tui.html](site/docs/tui.html#tui-keys).

Code: `client/internal/cli/keymap.go` (the configurable actions and the
conflict rules), `ui.go` (`globalKey`, `footerKeys`, `dangerKey`,
`overlayHelpKey`), `ui_overlay.go` (`overlayFor`).

## 1. Global keys

These work on every tab. They are the only keys a tab may not reuse.

| Key | Action | Configurable as |
|---|---|---|
| `q` | quit | `quit` |
| `r` | refresh the tab and the cluster summary | `refresh` |
| `y` | copy the current list | `copy` |
| `m` | mouse capture on/off | `toggle_mouse` |
| `c` | focus the context sidebar | `context_focus` |
| `?` | help (in an overlay: that overlay's keys first) | fixed |
| `` ` `` | client log viewer | fixed |
| `Tab`, `1`–`9` | next tab, jump to tab N | fixed |
| `j` `k` `g` `G`, arrows | move | fixed (vim aliases) |
| `Esc` | leave the overlay / dialog / sidebar | fixed |

`1`–`9` are reserved even where no tab exists yet (`reservedRunes`), so adding
a tab never shadows a binding. Inside overlays `quit` and `copy` keep working
(`dialogActions`): an overlay closes on Esc **and** on the quit key, and copies
with the copy key.

## 2. The detail verb: `Enter` / `i`

`Enter` opens the thing under the cursor; `i` opens its detail ("inspect").
On list tabs both lead to the same detail overlay (`Enter/i details`), on the
tree `Enter` expands or opens the container menu and `i` inspects. Several
detail overlays (node, context, forward detail, inspect) also close on the `i`
that opened them; every one closes on Esc. A new tab follows this: never give
`Enter` or `i` another meaning.

## 3. The commit idiom: `Ctrl-S`, alias `w`

Every editor that stages changes applies them with **`Ctrl-S`**, and footers
say `Ctrl-S apply` (secret and config dialogs keep their own verb: `Ctrl-S
create`, `Ctrl-S rotate`). Where a single letter is free, `w` is a one-key
alias: the staged list editors (`ui_listedit.go`), the node label editor and the
prune schedule editor. The vim editors (`ui_vimedit.go`) are the exception:
there `w` is the word motion in Normal mode and text in Insert mode, so they
take `Ctrl-S` in every mode plus `:w` / `:wq` / `:x` / `ZZ`. One-shot forms
(a tview `Form`) confirm with `Enter` on their button, which is not a staged
edit and stays as is.

An apply that changes a running service always goes through a confirm.

## 4. Dangerous keys are red, and uppercase

- A destructive key — delete, remove, stop, prune, and detach (which looks
  like bookkeeping but is a rolling update) — is wrapped in `dangerKey(label)`
  wherever it appears: `keyTag` then draws it red in the footer and
  `helpWriter.danger` in the `?` overlay. Wrap the **live** label
  (`dangerKey(keyLabel(u.km.VolDelete))`), so a remapped key is the one that
  turns red. `TestFooterDeleteKeyIsRedAndFollowsTheKeymap` and
  `TestDetachIsADangerKey` pin this.
- Destructive and heavyweight actions take a **capital** letter, one
  deliberate shifted key: `X` removes / deletes / stops on every tab
  (`service_remove`, `volume_delete`, `secret_delete`, `context_delete`,
  `forward_stop`), `P` prunes (volumes, nodes), `R` rotates a secret or rolls a
  service back, `M` measures reclaimable space now (an expensive scan). The
  converse does not hold: some capitals are merely a variant or a second verb
  (`S` reverse sort / prune schedule, `L` logs, `E` stack file, `A` attach).
- A destructive action is always behind a confirm, whatever its key.

## 5. Footers lead with Esc

An overlay's footer is built with `footerKeys(pairs...)`, which moves the Esc
pair (`Esc`, or `Esc/q` when it shares the quit key) to the front: on a narrow
terminal the end of the footer is what gets cut, and "how do I get out" must
survive (`TestOverlayFooterPutsEscFirst`). Overlay keys go into the footer,
never into a Box title: tview parses `[x]` in a title as a colour tag.
Overlays float over the main page via `centered` / `centeredGrow`, which keep
the two footer rows visible on short terminals.

## 6. `?` in every overlay

`overlayHelpKey` (in `appKeys`, ahead of the focused widget) opens the help
from any overlay whose focus is not a text input, listing that overlay's own
footer keys first. A new overlay gets this for free as long as it opens through
`u.overlayFor(page, back, footer)` — `TestOverlaysGoThroughTheOverlayType`
fails on any `pages.AddPage` outside the overlay type. Do not bind `?` to
anything else; in a text field it is a character.

## 7. How conflicts are kept out

Three tables in `keymap.go`, each checked against the source by a test:

- **`keyActions`** — every configurable action: its yaml name, the scopes
  (tabs) it is active in, its default rune. `loadKeybinds` refuses a
  `keys.yaml` override that clashes with another action sharing a scope
  (`conflictWith`), a reserved key, or a key from the two tables below.
  `TestDefaultKeybinds_NoPerTabCollision` keeps the defaults clash-free in
  every scope.
- **`dialogRunes`** — every fixed rune any dialog or detail view handles
  itself. `quit` and `copy` may not be remapped onto one (the dialog would
  answer with its own action). `TestDialogRunesMatchTheSource` greps the
  `ui*.go` rune literals: a fixed key added without being listed fails, and so
  does a listed key nothing uses any more. It also refuses a literal `'q'` or
  `'y'` — use `u.km.Quit` / `u.km.Copy` so a remap reaches the dialog.
- **`overlayKeysets`** — overlays that answer to configurable actions **next
  to** fixed keys of their own (the node detail answers to `node_prune` and has
  a fixed `M`). An action remapped onto one of that overlay's fixed keys is
  refused, because inside the overlay the fixed key wins.
  `TestOverlayKeysetsMatchTheSource` parses each declared handler with
  `go/ast` and compares its rune literals and the `km.*` fields it reads with
  the declaration, in both directions.

## 8. Adding a tab or an overlay

1. **Keys:** reuse a verb that exists (`n` new, `X` remove, `Enter`/`i`
   detail, `/` search, `s`/`S` sort, `a` all, `P` prune) before inventing one.
   Do not take a global key, `j`/`k`/`g`/`G` or a digit.
2. **Configurable or fixed:** a tab's own action keys are configurable — add a
   `keybinds` field and a `keyActions` entry with the tab's scope, then read
   `u.km.<Field>` everywhere (handler, footer, `helpSection`, empty state).
   Keys inside an overlay are fixed; add every new rune to `dialogRunes`.
3. **Overlay that also answers to a configurable action:** declare it in
   `overlayKeysets` (handler func name, fixed runes, actions).
4. **Open it with `u.overlayFor`**, show it with `centered`/`centeredGrow`,
   give it a `footerKeys` footer that includes Esc. If it edits, apply with
   `Ctrl-S` (and `w` where free) behind a confirm.
5. **Danger:** wrap destructive keys in `dangerKey` in the footer and use
   `h.danger` in the help section; default them to a capital.
6. **Docs:** the `?` help section of the tab, `site/docs/tui*.html`, and the
   `keys.yaml` example in `site/docs/tui.html#tui-keys` for a new action.
7. Run the keymap tests (`go test ./client/internal/cli -run 'Keybind|Keyset|DialogRunes|Overlay'`).
