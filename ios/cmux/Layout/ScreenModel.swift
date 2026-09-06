import Foundation
import UIKit

/// A surface's live screen as the bridge renders it: `rows` lines of styled
/// runs, updated by `surface.screen` pushes that carry only the lines that
/// changed. The bridge runs the terminal emulator; the phone just paints —
/// there is no emulator, no byte stream and no scroll view of its own here,
/// which is what makes this path consistent on a device. Rendering to an
/// attributed string is cached per (version, font size), so a card that
/// re-evaluates without a new update pays nothing.
@MainActor
final class ScreenModel: ObservableObject {
    /// Bumped on every applied update. The only published property: a card
    /// observes it to re-render, everything else is read on demand.
    @Published private(set) var version = 0
    private(set) var cols = 0
    private(set) var rows = 0
    private(set) var lines: [[ScreenRun]] = []
    private(set) var cursor: ScreenCursor?
    private var lastSeq = 0
    /// Asked for when an update's sequence shows a gap (a delta whose
    /// predecessor never arrived): the screen can no longer be trusted, and a
    /// resubscribe yields a fresh full update.
    var onResyncNeeded: (() -> Void)?

    private var cachedVersion = -1
    private var cachedFontSize: CGFloat = 0
    private var cachedAttributed: NSAttributedString?

    /// Rows from the top down to the last one with content — how many of the
    /// polled scrollback's trailing lines repeat what the live screen shows.
    var usedRows: Int {
        var used = 0
        for (index, runs) in lines.enumerated() where !runs.isEmpty {
            used = index + 1
        }
        return used
    }

    func apply(_ update: ScreenUpdate) {
        if !update.full && update.seq != lastSeq + 1 && lastSeq != 0 {
            onResyncNeeded?()
        }
        lastSeq = update.seq
        if update.full || update.cols != cols || update.rows != rows {
            cols = update.cols
            rows = update.rows
            lines = Array(repeating: [], count: max(0, rows))
        }
        for line in update.lines where line.i >= 0 && line.i < rows {
            lines[line.i] = line.runs
        }
        cursor = update.cursor
        version += 1
    }

    /// The screen as attributed text at `fontSize`, one paragraph per row.
    func attributed(fontSize: CGFloat) -> NSAttributedString {
        if let cachedAttributed, cachedVersion == version, cachedFontSize == fontSize {
            return cachedAttributed
        }
        let built = ScreenRenderer.render(lines: lines, cursor: cursor, fontSize: fontSize)
        cachedAttributed = built
        cachedVersion = version
        cachedFontSize = fontSize
        return built
    }
}

/// Turns rendered rows into an attributed string. UIKit-only; the model
/// above owns the cache.
enum ScreenRenderer {
    static let defaultForeground = UIColor.white.withAlphaComponent(0.85)
    static let defaultBackground = UIColor.black

    static func render(lines: [[ScreenRun]], cursor: ScreenCursor?, fontSize: CGFloat) -> NSAttributedString {
        let paragraph = NSMutableParagraphStyle()
        paragraph.lineSpacing = 1
        let regular = UIFont.monospacedSystemFont(ofSize: fontSize, weight: .regular)
        let bold = UIFont.monospacedSystemFont(ofSize: fontSize, weight: .bold)
        let out = NSMutableAttributedString()
        for (row, runs) in lines.enumerated() {
            let line = NSMutableAttributedString()
            for run in runs {
                var attrs: [NSAttributedString.Key: Any] = [.paragraphStyle: paragraph]
                let flags = run.a ?? 0
                attrs[.font] = (flags & 1) != 0 ? bold : regular
                var fg = run.fg.flatMap(color(hex:)) ?? defaultForeground
                var bg = run.bg.flatMap(color(hex:))
                if (flags & 16) != 0 { // inverse
                    let swapped = bg ?? defaultBackground
                    bg = fg
                    fg = swapped
                }
                if (flags & 8) != 0 { fg = fg.withAlphaComponent(fg.cgColor.alpha * 0.55) } // dim
                attrs[.foregroundColor] = fg
                if let bg { attrs[.backgroundColor] = bg }
                if (flags & 2) != 0 { attrs[.obliqueness] = 0.2 } // italic
                if (flags & 4) != 0 { attrs[.underlineStyle] = NSUnderlineStyle.single.rawValue }
                if (flags & 32) != 0 { attrs[.strikethroughStyle] = NSUnderlineStyle.single.rawValue }
                line.append(NSAttributedString(string: run.t, attributes: attrs))
            }
            if let cursor, cursor.visible, cursor.y == row {
                drawCursor(on: line, at: cursor.x, font: regular, paragraph: paragraph)
            }
            out.append(line)
            if row < lines.count - 1 {
                out.append(NSAttributedString(string: "\n", attributes: [.font: regular, .paragraphStyle: paragraph]))
            }
        }
        return out
    }

    /// A block cursor: inverts the cell under it, padding the row with spaces
    /// when the cursor sits past the end of its content.
    private static func drawCursor(on line: NSMutableAttributedString, at x: Int, font: UIFont, paragraph: NSParagraphStyle) {
        let length = line.length
        if x >= length {
            line.append(NSAttributedString(
                string: String(repeating: " ", count: x - length + 1),
                attributes: [.font: font, .paragraphStyle: paragraph, .foregroundColor: defaultForeground]
            ))
        }
        let range = NSRange(location: x, length: 1)
        line.addAttributes([
            .backgroundColor: UIColor.white.withAlphaComponent(0.75),
            .foregroundColor: UIColor.black,
        ], range: range)
    }

    static func color(hex: String) -> UIColor? {
        var s = Substring(hex)
        if s.hasPrefix("#") { s = s.dropFirst() }
        guard s.count == 6, let v = UInt32(s, radix: 16) else { return nil }
        return UIColor(
            red: CGFloat((v >> 16) & 0xff) / 255,
            green: CGFloat((v >> 8) & 0xff) / 255,
            blue: CGFloat(v & 0xff) / 255,
            alpha: 1
        )
    }
}

/// The cell size a monospaced font of `fontSize` points renders at, used to
/// turn a card's size into a "fit to phone" grid and to fit a screen's
/// columns into a card's width.
enum LiveCellMetrics {
    static let advanceOfMAt1pt: CGFloat = {
        let probeFont = UIFont.monospacedSystemFont(ofSize: 1, weight: .regular)
        return ("M" as NSString).size(withAttributes: [.font: probeFont]).width
    }()

    static func cell(fontSize: CGFloat) -> (width: CGFloat, height: CGFloat) {
        let font = UIFont.monospacedSystemFont(ofSize: fontSize, weight: .regular)
        let width = ("M" as NSString).size(withAttributes: [.font: font]).width
        // lineSpacing 1 matches the paragraph style the renderer uses.
        let height = ceil(font.lineHeight) + 1
        return (width, height)
    }
}
