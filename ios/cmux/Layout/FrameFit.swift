import CoreGraphics
import Foundation

/// Pure logic for the live terminal emulator card: sizing a monospaced font so
/// a frame's column count fits the card's width, and decoding the NDJSON
/// fixture frame files bundled for screenshot mode.
///
/// UIKit-free on purpose so both pieces are exercised by
/// `ios/scripts/test-frame-fit.swift` without booting a simulator.
enum FrameFit {
    /// The smallest and largest font sizes LivePaneView will pick. Below the
    /// minimum, glyphs become unreadable; above the maximum, a narrow card
    /// with few columns would otherwise blow up to a silly type size.
    static let minFontSize: CGFloat = 5
    static let maxFontSize: CGFloat = 14

    /// The point size a monospaced font must be set to so `columns` of it, at
    /// `advanceOfMAt1pt` points of horizontal advance per point of font size,
    /// exactly fill `viewWidth`. Clamped to [minFontSize, maxFontSize] — when
    /// clamped at the minimum the caller should let the emulator scroll
    /// horizontally rather than shrinking further.
    static func fontSize(
        forColumns columns: Int,
        viewWidth: CGFloat,
        advanceOfMAt1pt: CGFloat,
        min: CGFloat = minFontSize,
        max: CGFloat = maxFontSize
    ) -> CGFloat {
        guard columns > 0, viewWidth > 0, advanceOfMAt1pt > 0 else { return max }
        let raw = viewWidth / (CGFloat(columns) * advanceOfMAt1pt)
        if raw < min { return min }
        if raw > max { return max }
        return raw
    }

    /// Whether a font sized for `columns` at `viewWidth` has been clamped to
    /// the minimum — the signal that the card should scroll horizontally
    /// rather than trying to shrink text further.
    static func isClampedToMinimum(
        columns: Int,
        viewWidth: CGFloat,
        advanceOfMAt1pt: CGFloat,
        min: CGFloat = minFontSize
    ) -> Bool {
        guard columns > 0, viewWidth > 0, advanceOfMAt1pt > 0 else { return false }
        let raw = viewWidth / (CGFloat(columns) * advanceOfMAt1pt)
        return raw < min
    }

    /// Bounds for a "fit to phone" grid: narrower than 20 columns breaks every
    /// TUI, and herdr's PTY size is a u16 — 500 is already absurd on a phone.
    static let minFitColumns = 20
    static let maxFitColumns = 500
    static let minFitRows = 5
    static let maxFitRows = 500

    /// The terminal grid that fills `size` at a cell of `cellWidth` ×
    /// `cellHeight` points — what "fit to phone" asks the runtime to resize
    /// the real pane to. Clamped to the bounds above.
    static func gridToFit(size: CGSize, cellWidth: CGFloat, cellHeight: CGFloat) -> (columns: Int, rows: Int) {
        guard size.width > 0, size.height > 0, cellWidth > 0, cellHeight > 0 else {
            return (minFitColumns, minFitRows)
        }
        let columns = Swift.min(maxFitColumns, Swift.max(minFitColumns, Int(size.width / cellWidth)))
        let rows = Swift.min(maxFitRows, Swift.max(minFitRows, Int(size.height / cellHeight)))
        return (columns, rows)
    }

    /// The part of a polled screen read that belongs ABOVE a live emulator.
    /// `surface.read_text` returns scrollback *and* the visible screen, and the
    /// emulator already shows the screen, so the last `usedRows` lines (the
    /// rows the emulator currently has content in — the read trims trailing
    /// blank rows the same way) are dropped, along with any blank lines they
    /// leave behind. `usedRows` of 0 (no frame yet) leaves the text as is.
    static func historyAboveLiveScreen(polledText: String, usedRows: Int) -> String {
        guard usedRows > 0, !polledText.isEmpty else { return polledText }
        var lines = polledText.split(separator: "\n", omittingEmptySubsequences: false)
        guard lines.count > usedRows else { return "" }
        lines.removeLast(usedRows)
        while lines.last?.isEmpty == true {
            lines.removeLast()
        }
        return lines.joined(separator: "\n")
    }

    /// One line of a `frames-*.ndjson` fixture file — a recorded
    /// `surface.frame` push with no envelope (no `surface_id`, since the
    /// fixture is loaded under whatever surface id the caller assigns it to).
    struct FixtureFrameRecord: Decodable, Equatable {
        let type: String
        let seq: Int
        let width: Int
        let height: Int
        let encoding: String
        let full: Bool
        let bytes: String
    }

    /// Decodes an NDJSON fixture file's contents into its frame records.
    /// Blank lines are skipped; a line that fails to parse is dropped rather
    /// than aborting the whole file, so one bad line doesn't blank the card.
    static func decodeFixtureFrames(ndjson: String) -> [FixtureFrameRecord] {
        let decoder = JSONDecoder()
        return ndjson
            .split(separator: "\n", omittingEmptySubsequences: true)
            .compactMap { line -> FixtureFrameRecord? in
                let trimmed = line.trimmingCharacters(in: .whitespaces)
                guard !trimmed.isEmpty, let data = trimmed.data(using: .utf8) else { return nil }
                return try? decoder.decode(FixtureFrameRecord.self, from: data)
            }
    }
}
