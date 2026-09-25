# Quetzal brand

Quetzal is named after *Quetzalcoatlus*, the largest known pterosaur, with a
wingspan of about ten metres. The logo is that giant in flight: an anthracite
body and rust wings, set on cream.

## Files

| Use | Light background | Dark background |
|---|---|---|
| Logo with the name (default) | [`quetzal-lockup.svg`](quetzal-lockup.svg) | [`quetzal-lockup-dark.svg`](quetzal-lockup-dark.svg) |
| Logo with the name, square formats | [`quetzal-lockup-stacked.svg`](quetzal-lockup-stacked.svg) | [`quetzal-lockup-stacked-dark.svg`](quetzal-lockup-stacked-dark.svg) |
| Logo with the name, bird first | [`quetzal-lockup-bird-first.svg`](quetzal-lockup-bird-first.svg) | [`quetzal-lockup-bird-first-dark.svg`](quetzal-lockup-bird-first-dark.svg) |
| Bird alone | [`quetzal-logo.svg`](quetzal-logo.svg) | [`quetzal-logo-dark.svg`](quetzal-logo-dark.svg) |
| Bird on its cream square | [`quetzal-logo-square.svg`](quetzal-logo-square.svg) | same file |
| Favicon | [`favicon.svg`](favicon.svg), `favicon-16.png`, `favicon-32.png`, `favicon-512.png`, `apple-touch-icon.png` | same files |
| Social preview (1280×640) | [`social-preview.png`](social-preview.png) | [`social-preview-dark.png`](social-preview-dark.png) |

- **Default to the name first, with the bird on the right**, so the bird flies
  toward the name. Use the stacked version for square spaces and the bird-first
  version only when the layout calls for it.
- **On a dark background, use the `-dark` files.** They add a hairline white
  outline around the bird and set the name in cream. The anthracite body
  disappears on a dark background, and the outline has no place on a light one.
- **Minimum sizes:** the bird alone at 48 px tall, the logo with the name at
  140 px wide. Below that, use the favicon.
- **Clear space:** around the logo with the name, keep at least the height of
  its capital letters. Around the bird alone, keep the height of its head.
- **Don't** recolour, flip, rotate or stretch the logo, add a shadow or a
  gradient, or set the name next to the bird yourself.

## Colours

| Name | Hex | Role |
|---|---|---|
| Anthracite | `#2B2B2E` | The body; the name on light backgrounds |
| Rust | `#C8553D` | The wings |
| Cream | `#F3EDE4` | The background, the thin lines between the pieces; the name on dark backgrounds |
| Amber | `#E0A458` | The small touch on the belly |

The panel's interface colours derive from these. The rust becomes a lighter
accent that stays readable on a dark background. See the tokens at the top of
[`web/src/styles.css`](../../web/src/styles.css).

## Type

- **Bricolage Grotesque**: the name in the logo, headings.
- **Instrument Sans**: the interface.
- **JetBrains Mono**: the console, commands and addresses.

All three are under the SIL Open Font License. The panel bundles them in
[`web/public/fonts`](../../web/public/fonts) and loads nothing from a CDN.
