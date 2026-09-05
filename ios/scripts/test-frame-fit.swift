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

if failures > 0 {
    print("\n\(failures) failure(s)")
    exit(1)
}
print("\nall passed")
}
}
