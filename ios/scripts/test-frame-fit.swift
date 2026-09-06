// Tests for FrameFit — the live terminal card's font-fit math and fixture
// NDJSON decoding.
//
// Run via ./ios/scripts/run-tests.sh. UIKit-free on purpose, per the other
// suites in this directory.
import CoreGraphics
import Foundation

private var failures = 0

private func check(_ condition: Bool, _ what: String) {
    if condition {
        print("  ok   \(what)")
    } else {
        failures += 1
        print("  FAIL \(what)")
    }
}

private func approxEqual(_ a: CGFloat, _ b: CGFloat, tolerance: CGFloat = 0.001) -> Bool {
    abs(a - b) <= tolerance
}

@main
enum FrameFitTests {
static func main() {

print("FrameFit.fontSize")
// A monospaced "M" advance of 0.6pt per point of font size, 80 columns, a
// 480pt-wide card: the exact fit is 480 / (80 * 0.6) = 10pt.
check(approxEqual(FrameFit.fontSize(forColumns: 80, viewWidth: 480, advanceOfMAt1pt: 0.6), 10),
      "exact fit lands on the computed size")
check(FrameFit.fontSize(forColumns: 200, viewWidth: 300, advanceOfMAt1pt: 0.6) == FrameFit.minFontSize,
      "a huge column count clamps to the minimum instead of vanishing")
check(FrameFit.fontSize(forColumns: 10, viewWidth: 1000, advanceOfMAt1pt: 0.6) == FrameFit.maxFontSize,
      "a tiny column count clamps to the maximum instead of ballooning")
check(FrameFit.fontSize(forColumns: 0, viewWidth: 480, advanceOfMAt1pt: 0.6) == FrameFit.maxFontSize,
      "zero columns (no frame yet) falls back to the maximum rather than crashing")
check(FrameFit.fontSize(forColumns: 80, viewWidth: 0, advanceOfMAt1pt: 0.6) == FrameFit.maxFontSize,
      "zero width (not laid out yet) falls back to the maximum")
check(FrameFit.fontSize(forColumns: 85, viewWidth: 400, advanceOfMAt1pt: 0) == FrameFit.maxFontSize,
      "a zero advance (unmeasured font) falls back rather than dividing by zero")

print("FrameFit.isClampedToMinimum")
check(FrameFit.isClampedToMinimum(columns: 200, viewWidth: 300, advanceOfMAt1pt: 0.6),
      "the same huge column count reports as clamped")
check(!FrameFit.isClampedToMinimum(columns: 80, viewWidth: 480, advanceOfMAt1pt: 0.6),
      "a comfortably fitting frame does not report as clamped")

print("FrameFit.decodeFixtureFrames")
let ndjson = """
{"type":"terminal.frame","seq":1,"width":85,"height":19,"encoding":"ansi","full":true,"bytes":"aGVsbG8="}
{"type":"terminal.frame","seq":2,"width":85,"height":19,"encoding":"ansi","full":false,"bytes":"d29ybGQ="}

not json at all
{"type":"terminal.frame","seq":3,"width":85,"height":19,"encoding":"ansi","full":false,"bytes":"IQ=="}
"""
let records = FrameFit.decodeFixtureFrames(ndjson: ndjson)
check(records.count == 3, "malformed lines are dropped, valid ones kept (got \(records.count))")
check(records.first?.seq == 1 && records.first?.full == true,
      "first record decodes seq and full correctly")
check(records.last?.seq == 3 && records.last?.bytes == "IQ==",
      "later records after a bad line still decode")
check(FrameFit.decodeFixtureFrames(ndjson: "").isEmpty, "empty input decodes to no records")

print("FrameFit.historyAboveLiveScreen")
check(FrameFit.historyAboveLiveScreen(polledText: "a\nb\nc\nd", usedRows: 2) == "a\nb",
      "drops the rows the emulator shows")
check(FrameFit.historyAboveLiveScreen(polledText: "a\nb", usedRows: 2) == "",
      "nothing above the screen when the read is only the screen")
check(FrameFit.historyAboveLiveScreen(polledText: "a\nb", usedRows: 5) == "",
      "a read shorter than the screen has no history")
check(FrameFit.historyAboveLiveScreen(polledText: "a\n\n\nx\ny", usedRows: 2) == "a",
      "blank lines left at the cut are trimmed")
check(FrameFit.historyAboveLiveScreen(polledText: "a\nb", usedRows: 0) == "a\nb",
      "no frame yet leaves the text untouched")

print("FrameFit.gridToFit")
check(FrameFit.gridToFit(size: CGSize(width: 400, height: 800), cellWidth: 5, cellHeight: 10) == (80, 80),
      "divides the card by the cell size, rounding down")
check(FrameFit.gridToFit(size: CGSize(width: 50, height: 20), cellWidth: 5, cellHeight: 10) == (20, 5),
      "clamps to the minimum usable grid")
check(FrameFit.gridToFit(size: CGSize(width: 100000, height: 100000), cellWidth: 5, cellHeight: 10) == (500, 500),
      "clamps to herdr's maximum")
check(FrameFit.gridToFit(size: .zero, cellWidth: 5, cellHeight: 10) == (20, 5),
      "no size yet yields the minimum, never zero")

if failures > 0 {
    print("\n\(failures) failure(s)")
    exit(1)
}
print("\nall passed")
}
}
