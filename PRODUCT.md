# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

People copying large Android photo and file collections to their computer or an attached drive, including interrupted transfers that need to resume.

## Product Purpose

Android Transfer SLOP copies selected phone-folder contents through ADB and verifies local copies against source SHA-256 hashes. Transfer and verification preserve originals; explicitly confirmed Safe Source Delete removes only source files with freshly verified destination copies.

## Operating Context

A local Go server serves a browser interface at 127.0.0.1. The phone connects over USB with debugging authorized. The native destination picker is macOS-only; absolute destination paths can also be entered. Closing the browser does not stop the server's transfer.

## Capabilities and Constraints

- Select a phone, one source folder, and a local destination in the browser.
- Browse phone folders, configure batch limits, start/resume, stop safely, verify only, and download verification reports.
- Preserve paths relative to the selected source folder, not its ancestors.
- Treat the phone as authoritative: skip matching copies and replace mismatches only after a temporary copy is flushed and verified. Failed replacements retain the existing copy.
- Rehash existing files on restart; no cached success database or partial-file continuation.
- Verify-only reports problems without repairing copies. Extra destination files and phone originals are left alone.
- Safe Source Delete is a separate GUI action with destructive confirmation, fresh source/destination hashing, durable local copies, path/identity guards, stop controls, and a downloadable deletion report. It ignores transfer batch limits and never repairs destination files. Both folders must stay idle: ADB cannot make verification and removal atomic.
- Destination storage requires advisory locks and durable file operations. Filesystems without hard links use an exclusive-copy fallback for new files.
- Preserve the existing Go, HTML, CSS, and vanilla JavaScript implementation and local-only security model.

## Brand Commitments

The user requests a bright, modern app. The user selected setup and progress together: source, destination, and prominent live progress should remain visible together rather than becoming a wizard.

## Evidence on Hand

README.md, existing application code, regression tests, and a runnable local interface. No marketing metrics, testimonials, or real-device transfer-speed claims are supplied.

## Product Principles

- Keep phone originals unchanged unless the user explicitly confirms Safe Source Delete and current destination verification succeeds.
- Verify before accepting a new or replacement copy.
- Make source, destination, and run state unambiguous.
- Preserve the distinction between transfer, verification, and source deletion.
