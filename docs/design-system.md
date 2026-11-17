# Design system of the UI

How the internal UI ([ui.md](ui.md)) looks and why. The tokens live in `internal/ui/static/ui.css`; this page is
their reference. Decisions: D70–D72 in [decisions.md](decisions.md).

## 1. The idea: a relay interlocking's panel

Postwerk is a relay. Railways have run on relays too: a **relay interlocking** (German *Relaisstellwerk*) sets the
route of each train, and the signal box shows every line on one panel. The panel is grey, the tracks are drawn in
graphite, and lamps light up where something happens: a route that is set, a track that is occupied, a signal at
stop. A quiet panel means that all is well; the operator looks where a lamp is lit.

That is the operator's job in Postwerk too. A mail comes in on one line and goes out on one route per target, and the
operator wants to see at a glance which route has a problem. So the UI borrows the panel's rules, not its looks:

- **Colour is reserved for state.** Surfaces, text and controls are greys and graphite. Green, red, amber, blue and
  purple appear only as the lamp of a status or the tint of a notice that is about that status. A healthy page is
  calm grey with green lamps; a broken one shows its colour where the problem is.
- **A lamp has a shape as well as a colour,** so a state reads without colour vision (section 3).
- **One line in, one branch per target.** The logo, the route on a mail's page and the target lines on the overview
  draw the same picture.

Why this and not a post office: a postal look leads to envelopes, stamps and the yellow and horn of national post
companies, which Postwerk must not imitate. A signal box is about routing and watching state, which is what the
operator does here, and it fits the name (*Stellwerk*, *Postwerk*).

## 2. Palette

All pairs below pass WCAG AA: text 4.5:1 or more on every surface it is used on, lines and lamps 3:1 where they
carry meaning (a lamp always has a rim in its text colour).

**Light: the panel by day**

| Token | Hex | Role |
|---|---|---|
| `--bg` | `#e3e6e1` | the panel the page sits on |
| `--surface` | `#f7f8f5` | a tile: cards, tables, forms, the header |
| `--surface-2` | `#edefeb` | sunken: hovered rows, closed tags |
| `--line` | `#cdd2cc` | hairlines between rows and around tiles |
| `--line-strong` | `#8f9791` | table heads, input borders, route lines |
| `--text` | `#1c201e` | graphite: text, links, focus ring (15.5:1 on surface) |
| `--muted` | `#555d58` | labels and secondary text (6.4:1 on surface, 5.4:1 on bg) |
| `--ink` | `#262b28` | primary buttons, the logo tile |
| `--on-ink` | `#f7f8f5` | text on `--ink` |

**Dark: the panel at night**

| Token | Hex | Role |
|---|---|---|
| `--bg` | `#1b1f1d` | the panel |
| `--surface` | `#232825` | a tile |
| `--surface-2` | `#2b312d` | sunken |
| `--line` | `#3a423d` | hairlines |
| `--line-strong` | `#5e6862` | table heads, input borders, route lines |
| `--text` | `#e4e8e3` | text, links, focus ring (12.1:1 on surface) |
| `--muted` | `#a0a9a2` | labels and secondary text (6.2:1) |
| `--ink` | `#e4e8e3` | primary buttons are light in the dark |
| `--on-ink` | `#1b1f1d` | text on `--ink` |

Links are graphite with an underline in `--line-strong` that darkens on hover. There is no brand accent colour: the
graphite of the tracks is the brand.

## 3. Status colours and lamps

Every derived status (architecture.md) has three tokens: `--<status>-fg` for its word and the lamp's rim,
`--<status>-lamp` for the lamp's fill, `--<status>-bg` for a tinted area. Status names are the store's, so
`--handed_off-fg` goes with the class `st-handed_off`.

| Status | Lamp | Light fg / lamp | Dark fg / lamp | Meaning on the panel |
|---|---|---|---|---|
| queued | empty ring | `#3e4642` / none | `#c9d0ca` / none | waiting at the signal |
| sending | ring with a lit centre | `#1f5fa8` / `#3d86d9` | `#7fb2f0` / `#5e9fe6` | route set, moving |
| handed off | lit circle | `#1e7340` / `#2e9e5b` | `#5cc489` / `#45b572` | arrived |
| failed | square | `#b42318` / `#d7371f` | `#ff8a73` / `#f0553b` | stop |
| unknown | diamond | `#7f5300` / `#eab21a` | `#e6b54a` / `#f2c14a` | caution: check |
| partial | half lit circle | `#8e3b78` / `#b4529c` | `#e08fcb` / `#d07abd` | some arrived, some not |
| skipped | short bar | `#5a625d` / `#a3aaa5` | `#9aa29c` / `#6e7771` | not for this route |

The words of the calm states (queued, handed off, skipped) stay in text colour; only their lamp is coloured. Sending,
failed, unknown and partial colour their word as well, so problems stand out in a long list.

The hues follow the Okabe–Ito set for colour-vision deficiency. Simulated for deuteranopia, protanopia and
tritanopia, failed and unknown stay apart by lightness (amber lamp is light, red is dark); green and red, and blue and
purple, come close for protanopia, and there the shapes (circle against square, ring against half) tell them apart.
The word is always there as well.

Health of a target uses the same lamps: up is a lit circle in the handed-off green, down a square in the failed red.
Sender keys too (D79): an active key or sender is the lit circle, an expired key the amber diamond (the application
probably fails to log in), a revoked key or a sender with every key revoked the calm bar of skipped. A key that expires
within a week gets an amber tag.

For charts (inline SVG, filled by class, never by a `style` attribute because of the CSP): fill with the
`-lamp` tokens, `--line-strong` for queued; gridlines `--chart-grid`, axes `--chart-axis`.

## 4. Typography

**Barlow** for everything, **Barlow Semi Condensed** for labels; the system monospace for data that is code: mail ids,
SMTP codes, event kinds, addresses, header values.

Barlow is a grotesk drawn after California's public signs, buses and trains: plain, slightly rounded, made to be read
at a glance, which suits a panel. The semi-condensed cut keeps labels and table heads short. Both are under the SIL
Open Font License 1.1; three files of the Latin subset (400 and 600 of Barlow, 500 of Barlow Semi Condensed, about
22 KB each) are embedded in the binary and served by the UI itself (`font-src 'self'`), with the licence in
`internal/ui/static/OFL.txt`. Nothing loads from outside. If the fonts do not load, the stack falls back to
`system-ui`.

| Token | Size | Use |
|---|---|---|
| `--fs-sm` | 13 px | labels (semi condensed 500), small lamps, notes |
| `--fs-table` | 15 px | tables, timeline |
| `--fs-body` | 16 px | text, forms |
| `--fs-h3` | 18 px | card titles, summary values |
| `--fs-h2` | 21 px | target names, sections |
| `--fs-h1` | 26 px | page title (22 px on phones) |

Weights: 400 for text, 600 for headings, buttons, values and strong text. Figures are tabular wherever they are
compared (`.num`). Labels are in sentence case; there are no all-caps labels.

## 5. Space and corners

Space comes in 4 px steps: `--sp-1` 4, `--sp-2` 8, `--sp-3` 12, `--sp-4` 16, `--sp-5` 24, `--sp-6` 32, `--sp-7` 48.
Tight inside a group, one step more between groups, more above a heading than below it.

Corners are almost square, like panel tiles: `--r-2` 4 px for tiles, inputs and buttons, `--r-1` 2 px for tags and
the focus ring. There are no shadows; tiles are set apart by a 1 px `--line` and by the darker panel around them.
The page is at most `--page` (78 rem) wide.

## 6. Components

- **Header.** The logo and name, the instance as a plate with a graphite border (so staging and production are told
  apart at once), the navigation, the account. The current page has a 3 px graphite bar under it, like a set route.
- **Summary board** (overview). Three parts in one tile: targets, queue, recipients of the last 24 hours, each a lamp,
  a label and a short sentence with links to the matching list. It comes before any detail.
- **Target line** (overview). One tile per target: name and health, three readouts (queued now, oldest queued,
  recipients in 24 h), a bar of recipients by status and the counts with their lamps. Zero counts are dimmed.
- **Route** (mail page). A graphite square for the mail, then one branch per target with its status and a link to its
  part of the page.
- **Tables.** Dense rows with 1 px lines, sentence-case heads in the label face over a stronger line, figures right
  aligned and tabular, a faint hover. On phones the mail list and the recipient tables become one block per row.
- **Status.** `.st` is a lamp and a word; `.st-sm` in tight places, `.st-lg` with a tint and border at the head of a
  page or delivery.
- **Tags** (`.tag`): bordered, not filled: optional, not configured, tokens file, senders file, database, closed.
- **Timeline** (events). Time on the left, a mark on a vertical line, the level, the event kind, where it happened
  (target, recipient, attempt) and the reply. The day is shown once and again when it changes. Info marks are empty
  rings, warnings amber diamonds, errors red squares.
- **A key shown once.** A read-only field in monospace on the sunken surface with a graphite border, wrapping the
  whole key, under a warning notice that it is not shown again; nothing to click, the operator selects and copies.
- **Forms.** Labels above fields; fields with a strong border that turns graphite on hover and focus. Primary button
  graphite (`--ink`), secondary outlined, danger in the failed red. A disabled button is grey with its reason next to
  it. Resolve options are `<details>` that open on a click, so the page needs no JavaScript.
- **Throughput chart** (statistics). Stacked bars of mails by state, filled with the `-lamp` tokens (in flight with
  `--line-strong`), grid lines `--chart-grid`, axis `--chart-axis`, labels in the label face in `--muted`; the time a
  target was down is a band in `--failed-bg` with a `--failed-lamp` mark on top. Neutral summary values get no lamp.
- **Notices.** A tinted box in the colour of what it reports: ok, error, warning.
- **Empty states** say what the empty place means and what to do: "No mail matches this filter. Reset the filter."
- **Focus.** A 2 px graphite (light in the dark) outline with 2 px offset on every control; a skip link to the
  content. Motion is limited to the Resolve arrow turning (150 ms) and is off with `prefers-reduced-motion`.

## 7. Logo

A track plan in a graphite tile: one line comes in from the left and branches twice into three lines, each ending in a
lamp. It is the product in one picture: one mail, one copy per target, each with its state. The strokes are thick and
on a 64 grid, so it holds at 16 px (favicon) and at 72 px (README). Files:

- `docs/logo.svg` and `internal/ui/static/logo.svg`: graphite tile `#262b28`, lines `#eef1ec`. Works on light and
  dark backgrounds.
- `docs/logo-mono.svg`: the lines and lamps alone in graphite, the one-colour version for light backgrounds; recolour
  it as a whole, never in parts.

Keep space of a quarter of its width around it. Do not add colour to single lamps, round the track ends, or put it on
an envelope.

## 8. Do and don't

Do:

- Show state as lamp and word, and put the summary before the detail.
- Keep colour for state; a new feature that needs colour takes a status token, not a new hue.
- Use the tokens; new tokens get a role here before they are used.
- Check every new pair of colours for AA in light and dark, and check a new page at 390 px width.

Don't:

- No teal, no purple gradients, no acid green on black, no cream paper. No gradients or shadows as decoration.
- No emoji or Unicode symbols as icons; draw a lamp or an SVG.
- No big rounded corners, no cards inside cards, no all-caps labels.
- No monospace for ordinary text; only for ids, codes and addresses.
- Nothing that resembles a postal or railway company's identity: no post horn, no post yellow, no company's red.
- No inline `style` attributes or scripts: the CSP forbids them (`style-src 'self'`, no `script-src`).
