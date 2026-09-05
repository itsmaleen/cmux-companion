// Tests for ScrollFollow — when a terminal card keeps following new output.
//
// Run via ./ios/scripts/run-tests.sh. UIKit-free on purpose: the rules below are
// the ones that were wrong, and they must be checkable without the app.
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

@main
enum ScrollFollowTests {
static func main() {

print("ScrollFollow.isAtBottom")
// A 1000pt document in a 400pt viewport ends at offset 600.
check(ScrollFollow.isAtBottom(offsetY: 600, contentHeight: 1000, viewportHeight: 400),
      "exactly at the end counts as bottom")
check(ScrollFollow.isAtBottom(offsetY: 580, contentHeight: 1000, viewportHeight: 400),
      "within the threshold counts as bottom")
check(!ScrollFollow.isAtBottom(offsetY: 400, contentHeight: 1000, viewportHeight: 400),
      "scrolled well up is not bottom")
check(ScrollFollow.isAtBottom(offsetY: 0, contentHeight: 200, viewportHeight: 400),
      "content shorter than the viewport is always at bottom")

print("ScrollFollow.bottomOffset")
check(ScrollFollow.bottomOffset(contentHeight: 1000, viewportHeight: 400, bottomInset: 0) == 600,
      "target offset puts the end at the viewport bottom")
check(ScrollFollow.bottomOffset(contentHeight: 1000, viewportHeight: 400, bottomInset: 34) == 634,
      "bottom inset (home indicator / keyboard) is included")
check(ScrollFollow.bottomOffset(contentHeight: 100, viewportHeight: 400, bottomInset: 0) == 0,
      "never scrolls to a negative offset")

print("ScrollFollow.follow")
check(ScrollFollow.follow(current: true, atBottom: false, isProgrammatic: false) == false,
      "a finger scrolling up stops following")
check(ScrollFollow.follow(current: false, atBottom: true, isProgrammatic: false) == true,
      "a finger returning to the bottom resumes following")
check(ScrollFollow.follow(current: true, atBottom: false, isProgrammatic: true) == true,
      "a programmatic scroll that lands short does NOT stop following")
check(ScrollFollow.follow(current: false, atBottom: true, isProgrammatic: true) == false,
      "a programmatic scroll does not start following either")

// The reported regression, as a sequence: a card following live output, a poll
// whose scroll-to-bottom lands short because layout was mid-animation, then more
// output arriving. Before the fix step 2 latched `following` to false and every
// later poll left the card stranded mid-output.
print("regression: a short programmatic landing must not strand the card")
var following = true
// 1. poll lands the card at the true bottom
following = ScrollFollow.follow(current: following, atBottom: true, isProgrammatic: true)
// 2. card is mid-animation; the computed target is short of the real bottom
following = ScrollFollow.follow(current: following, atBottom: false, isProgrammatic: true)
check(following, "still following after a short landing")
// 3. the correcting second pass reaches the real bottom
following = ScrollFollow.follow(current: following, atBottom: true, isProgrammatic: true)
check(following, "still following after the correction")
// 4. now the user deliberately scrolls up to read back
following = ScrollFollow.follow(current: following, atBottom: false, isProgrammatic: false)
check(!following, "the user's own scroll still stops the follow")

print("ScrollFollow.anchor")
check(ScrollFollow.anchor(autoScroll: true, oldText: "a\nb\nc", newText: "a\nb\nc\nd") == .followBottom,
      "following live output always follows the bottom")
check(ScrollFollow.anchor(autoScroll: true, oldText: "", newText: "a") == .followBottom,
      "following live output follows the bottom even from empty")
check(ScrollFollow.anchor(autoScroll: false, oldText: "", newText: "a\nb\nc") == .followBottom,
      "the very first text (nothing to anchor to) follows the bottom")
check(ScrollFollow.anchor(autoScroll: false, oldText: "a\nb\nc", newText: "a\nb\nc") == .keepOffset,
      "identical text keeps the raw offset")
check(ScrollFollow.anchor(autoScroll: false, oldText: "a\nb\nc", newText: "a\nb\nc\nd\ne") == .keepOffset,
      "new lines appended at the bottom keep the raw offset — nothing above the viewport moved")
check(ScrollFollow.anchor(autoScroll: false, oldText: "b\nc\nd", newText: "a\nb\nc\nd") == .shiftByAddedHeight,
      "history prepended above shifts the offset by the added height")
check(ScrollFollow.anchor(autoScroll: false, oldText: "a\nb\nc", newText: "x\ny\nz") == .keepDistanceFromBottom,
      "an unrelated replacement (full-screen repaint) keeps the distance from the bottom")
check(ScrollFollow.anchor(autoScroll: false, oldText: "line1\nline2\nline3", newText: "line1\nlineX\nline3") == .keepDistanceFromBottom,
      "a changed middle line (neither prefix nor suffix) keeps the distance from the bottom")
check(ScrollFollow.anchor(autoScroll: false, oldText: "foo   \nbar", newText: "foo\nbar") == .keepDistanceFromBottom,
      "a rewritten line (e.g. trailing whitespace trimmed) is neither prefix nor suffix, so it keeps distance from bottom")

if failures > 0 {
    print("\n\(failures) failure(s)")
    exit(1)
}
print("\nall passed")
}
}
