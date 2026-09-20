---
name: Android Transfer SLOP
description: A bright verification workbench for phone-to-computer transfers.
colors:
  background: "#f3f7fc"
  surface: "#ffffff"
  ink: "#18334d"
  muted: "#52677d"
  primary: "#1760d6"
  primary-hover: "#104eaf"
  primary-soft: "#e9f1ff"
  status-surface: "#e7f0fc"
  success: "#146850"
  success-soft: "#ddf5ef"
  error: "#b23335"
  border: "#dce5ef"
typography:
  body:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif'
    fontSize: "16px"
    lineHeight: 1.5
  app-title:
    fontSize: "1.5rem"
    fontWeight: 700
    lineHeight: 1.3
    letterSpacing: "-0.035em"
  section-heading:
    fontSize: "1.375rem"
    fontWeight: 650
    lineHeight: 1.35
    letterSpacing: "-0.025em"
  status:
    fontSize: "1.3125rem"
    fontWeight: 600
    lineHeight: 1.45
    letterSpacing: "-0.025em"
  label:
    fontSize: "0.8125rem"
    fontWeight: 600
  help:
    fontSize: "0.8125rem"
    lineHeight: 1.6
rounded:
  control: "0.625rem"
  browser: "0.75rem"
  panel: "1rem"
spacing:
  compact: "0.625rem"
  base: "1rem"
  panel: "1.5rem"
  wide: "2rem"
components:
  button-primary:
    backgroundColor: "{colors.primary}"
    textColor: "{colors.surface}"
    rounded: "{rounded.control}"
    padding: "0.6875rem 1.375rem"
  button-primary-hover:
    backgroundColor: "{colors.primary-hover}"
  button-secondary:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    padding: "0.6875rem 1rem"
  input:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    padding: "0.6875rem 0.8125rem"
  status-panel:
    backgroundColor: "{colors.status-surface}"
    textColor: "{colors.ink}"
    rounded: "{rounded.panel}"
    padding: "1.5rem"
---

# Design System: Android Transfer SLOP

## Overview

**Creative North Star: "Verification workbench"**

A bright, modern utility with a white working surface, clear blue actions, and a softly tinted live-status region. The user approved setup and progress together rather than a wizard. Native UI typography keeps the interface familiar and self-contained, without remote fonts or image dependencies.

**Key Characteristics:**
- Clear source-to-destination hierarchy.
- Spacious controls with compact supporting text.
- Status communicated through words, counts, and control availability as well as color.

## Colors

Blue identifies actions and folder affordances. Navy carries primary text; muted blue-gray carries explanation. White separates editable work from the cool background. The status region uses pale blue, changing to mint for completed runs. Errors use red text and explicit descriptions, never color alone.

## Typography

Use the native UI stack throughout the operating interface. The app title, section heading, and live-status message form a modest hierarchy; controls remain at 0.875rem. Numeric counts and batch inputs use tabular numerals. Supporting text is smaller but has increased line height. Text generally stays within 72 characters per line; paths wrap where shown as status text.

## Layout

The workspace is bounded at 78rem. At wide desktop sizes, the setup form fills the flexible column beside a 21.5rem status rail. The rail is sticky with a 1.5rem top offset. Source and destination align in two columns, with a directional arrow between their headings. Advanced settings are a native disclosure below them.

At 72rem, outer spacing and the status rail narrow. At 62rem, source and destination stack within the desktop form. At 48rem, the whole workspace becomes one column and the status panel stops sticking. At 35rem, source and destination stack again and action buttons become full-width. Keep all controls available at every width.

## Elevation & Depth

The white form uses one neutral, offset ambient shadow: `0 8px 32px -12px rgb(0 0 0 / 12%)`. The status panel is separated by tone, not another shadow. Internal rules distinguish functional sections. Do not combine a panel border with its ambient shadow.

## Shapes

Panels have gently rounded corners, controls a tighter radius, and folder-browser chrome an intermediate radius. Icons are small, consistent outline SVGs. Circular status symbols support the written state rather than introducing independent controls.

## Components

### Buttons and fields

Controls are at least 2.875rem tall. The primary transfer action uses blue with white text; verification and folder selection are visually secondary. Disabled controls have explicit subdued foreground and background colors. Inputs retain persistent labels. Invalid fields use the error color, with associated error text.

Safe Source Delete sits beside Start / resume transfer, using the error color for its text and border and the existing error-notice tint (`#fff0ef`) on hover. It remains secondary to transfer and uses a native confirmation with the selected device and paths, irreversible consequences, and the idle-folder requirement. No run starts if confirmation is canceled. The control shares the settings fieldset's busy/disconnected locks.

### Focus and motion

Keyboard focus is a 3px blue outline with a 3px offset; constrained folder-browser controls place it inside the boundary. Selection and caret colors follow the action palette. A single 260ms workspace fade provides arrival; buttons use 140ms color transitions. Reduced-motion preference disables both.

### Folder browser

The browser expands inside the source pane. Breadcrumbs wrap, the folder list scrolls independently, and the selected path is reflected in the source field. Long folder names wrap. Pagination remains usable within a narrow source column.

### Status and activity

Live state is the dominant text in the status rail. Progress, counts, current file, errors, stop, and report controls preserve their existing behavior. Activity is a native disclosure with a contained scrolling log. Completed state uses mint; verification problems retain explicit counts and report access.

Deletion status distinguishes files checked, confirmed deleted, retained/unconfirmed, and not checked. Missing, mismatched, changed, and error counts remain explicit, with a deletion report download. Stopping never promises to restore deleted files, and uncertain device responses never claim a file was retained.

## Do's and Don'ts

- Do preserve visible labels, native disclosures, focus outlines, and reduced-motion behavior.
- Do keep transfer and verify-only consequences explicit.
- Do let long paths wrap without widening the page.
- Do show only real run state and measured counts.
- Don't add decorative navigation or invented dashboard metrics.
- Don't obscure progress behind transfer settings on desktop.
- Don't use remote fonts or decorative images for this local operating surface.
