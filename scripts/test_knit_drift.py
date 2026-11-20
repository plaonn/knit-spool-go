import tempfile
import unittest
from pathlib import Path

import knit_drift as kd

PINNED = """\
| Protocol version   | 1          |
| This revision      | 2026-08-30 |

## 6. The spool [Spool]

| **S-6.2-3** | A spool MUST echo the applied bounds. |
| **S-6.4-1** | A spool SHOULD shed idle scopes.      |
| **B-2-2**   | Unknown map keys MUST be ignored.     |
| **C-3.1-4** | A client keeps its scopes.            |
| **S-6.9-1** | A spool MAY log.                      |

## 13. Test vectors [Both]

vectorA = 00
vectorB = 01

## Appendix B. Change log

| Date       | Change              | Asks of implementers |
|------------|---------------------|----------------------|
| 2026-08-30 | **Old entry.** Text | None for spools.     |
"""

CURRENT = """\
| Protocol version   | 1          |
| This revision      | 2026-10-31 |

## 6. The spool [Spool]

| **S-6.2-3** | A spool MUST echo the applied   bounds. |
| **S-6.4-1** | A spool SHOULD shed idle scopes first.  |
| **B-2-2**   | Unknown map keys MUST be ignored.       |
| **C-3.1-4** | A client keeps its scopes for a while.  |
| **S-6.4-6** | A spool MAY cap subscribers; the cap MUST NOT apply to a room. |
| **S-7.8-1** | A spool advertises `sig`.               |
| **S-7.8-2** | A spool never answers a sig.            |

## 13. Test vectors [Both]

vectorA = 00
vectorB = 01
vectorC = 02

## Appendix B. Change log

| Date       | Change              | Asks of implementers |
|------------|---------------------|----------------------|
| 2026-08-30 | **Old entry.** Text | None for spools.     |
| 2026-10-28 | **A signal lane (§7.8: S-7.8-1 to S-7.8-2 new).** Text | **Spools:** optional; omit `sig`. **Clients:** ignore it |
| 2026-10-31 | **A spool may cap subscribers (§6.4: S-6.4-6 new).** Text | **Spools:** optional; no cap is unaffected. **Clients:** none |
"""


def reference(**overrides):
    values = dict(pinned_repo="getknit/knit-spool", canonical_repo="getknit/knit-spool", pinned_commit="a" * 40,
                  head_commit="a" * 40, commits_ahead=0, release_tag="", release_commit="",
                  release_contains_pin=True)
    values.update(overrides)
    return kd.ReferenceState(**values)


class ParseTest(unittest.TestCase):
    def test_header(self):
        self.assertEqual(kd.parse_header(PINNED), {"Protocol version": "1", "This revision": "2026-08-30"})

    def test_rules_normalise_whitespace(self):
        rules = kd.parse_rules(CURRENT)
        self.assertEqual(rules["S-6.2-3"], "A spool MUST echo the applied bounds.")
        self.assertIn("B-2-2", rules)

    def test_changelog_title_and_asks(self):
        entries = kd.parse_changelog(CURRENT)
        self.assertEqual([e["date"] for e in entries], ["2026-08-30", "2026-10-28", "2026-10-31"])
        self.assertEqual(entries[1]["title"], "A signal lane (§7.8: S-7.8-1 to S-7.8-2 new).")
        self.assertEqual(kd.spool_ask(entries[1]["asks"]), "optional; omit `sig`")
        self.assertEqual(kd.spool_ask(entries[0]["asks"]), "none")

    def test_keyword_level(self):
        self.assertEqual(kd.keyword_level("x MUST NOT y"), "MUST")
        self.assertEqual(kd.keyword_level("x SHOULD y"), "SHOULD")
        self.assertEqual(kd.keyword_level("x MAY y"), "MAY")
        self.assertEqual(kd.keyword_level("must, lowercase"), "informative")

    def test_lock(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "lock"
            path.write_text("# c\nSPEC_REPO=a\n")
            with self.assertRaisesRegex(ValueError, "missing"):
                kd.read_lock(path)
            path.write_text("SPEC_REPO\n")
            with self.assertRaisesRegex(ValueError, "KEY=VALUE"):
                kd.read_lock(path)

    def test_repository_lock_is_complete(self):
        lock = kd.read_lock(Path(__file__).resolve().parent.parent / "knit-reference.lock")
        self.assertRegex(lock["SPEC_COMMIT"], r"^[0-9a-f]{40}$")


class DiffTest(unittest.TestCase):
    def setUp(self):
        self.diff = kd.diff_spec(PINNED, CURRENT)

    def test_rule_changes(self):
        changes = {c.rule: c for c in self.diff.rule_changes}
        self.assertNotIn("S-6.2-3", changes, "whitespace-only edits are not changes")
        self.assertNotIn("B-2-2", changes)
        self.assertEqual((changes["S-6.4-1"].kind, changes["S-6.4-1"].keyword), ("changed", "SHOULD"))
        self.assertEqual((changes["S-6.4-6"].kind, changes["S-6.4-6"].keyword), ("added", "MUST"))
        self.assertEqual((changes["S-6.9-1"].kind, changes["S-6.9-1"].keyword), ("removed", "MAY"))
        self.assertEqual(changes["S-7.8-1"].keyword, "informative")
        self.assertEqual(self.diff.client_rule_changes, 1)
        self.assertEqual(list(changes), ["S-6.4-1", "S-6.4-6", "S-6.9-1", "S-7.8-1", "S-7.8-2"])

    def test_rule_asks_follow_changelog_ranges(self):
        changes = {c.rule: c for c in self.diff.rule_changes}
        self.assertEqual(changes["S-6.4-6"].spool_ask, "2026-10-31: optional; no cap is unaffected")
        self.assertEqual(changes["S-7.8-2"].spool_ask, "2026-10-28: optional; omit `sig`")
        self.assertEqual(changes["S-6.4-1"].spool_ask, "")

    def test_vectors_and_changelog(self):
        self.assertEqual((self.diff.vectors_removed_lines, self.diff.vectors_added_lines), (0, 1))
        self.assertEqual([e["date"] for e in self.diff.changelog], ["2026-10-28", "2026-10-31"])

    def test_changed_vector_is_must_review(self):
        diff = kd.diff_spec(PINNED, PINNED.replace("vectorB = 01", "vectorB = 03"))
        self.assertEqual(diff.vectors_removed_lines, 1)
        level, reasons = kd.grade(True, diff, reference())
        self.assertEqual(level, "must-review")
        self.assertIn("Section 13", reasons[0])

    def test_protocol_version_change_is_must_review(self):
        diff = kd.diff_spec(PINNED, PINNED.replace("| Protocol version   | 1 ", "| Protocol version   | 2 "))
        level, reasons = kd.grade(True, diff, reference())
        self.assertEqual(level, "must-review")
        self.assertIn("protocol version changed: 1 -> 2", reasons[0])


class GradeTest(unittest.TestCase):
    def test_no_drift(self):
        self.assertEqual(kd.grade(False, None, reference()), ("none", []))

    def test_spec_drift_levels(self):
        level, reasons = kd.grade(True, kd.diff_spec(PINNED, CURRENT), reference())
        self.assertEqual(level, "must-review")
        self.assertTrue(any(r.startswith("**review**") and "SHOULD" in r for r in reasons))

    def test_should_only_is_review(self):
        current = PINNED.replace("shed idle scopes.  ", "shed idle scopes now.")
        level, _ = kd.grade(True, kd.diff_spec(PINNED, current), reference())
        self.assertEqual(level, "review")

    def test_text_only_change_is_informational(self):
        level, reasons = kd.grade(True, kd.diff_spec(PINNED, PINNED + "\nprose\n"), reference())
        self.assertEqual(level, "informational")
        self.assertIn("outside rules", reasons[0])

    def test_reference_signals(self):
        self.assertEqual(kd.grade(False, None, reference(commits_ahead=3))[0], "informational")
        self.assertEqual(kd.grade(False, None, reference(canonical_repo="getknit/other"))[0], "review")
        level, reasons = kd.grade(False, None, reference(release_tag="v1", release_commit="b" * 40,
                                                         release_contains_pin=False))
        self.assertEqual(level, "review")
        self.assertIn("does not contain the pinned commit", reasons[0])
        self.assertEqual(kd.grade(False, None, reference(release_tag="v1", release_commit="a" * 40))[0], "none")
        self.assertEqual(kd.grade(False, None, reference(release_tag="v1", release_commit="a" * 40,
                                                         commits_ahead=18))[0], "none")


class RenderTest(unittest.TestCase):
    LOCK = {"SPEC_REPO": "getknit/knit", "SPEC_PATH": "docs/SPOOL_PROTOCOL.md", "SPEC_COMMIT": "c" * 40,
            "SPEC_REVISION": "2026-08-30", "PROTOCOL_VERSION": "1", "REFERENCE_REPO": "getknit/knit-spool",
            "REFERENCE_COMMIT": "a" * 40}

    def test_report_lists_rules_and_changelog(self):
        diff = kd.diff_spec(PINNED, CURRENT)
        level, reasons = kd.grade(True, diff, reference())
        report = kd.render(self.LOCK, "d" * 40, diff, reference(), level, reasons)
        self.assertIn("Overall level: **must-review**", report)
        self.assertIn("revision `2026-10-31`", report)
        self.assertIn("- **S-6.4-6** added, MUST", report)
        self.assertIn("change log asks spools: 2026-10-31: optional; no cap is unaffected", report)
        self.assertIn("| 2026-10-31 | A spool may cap subscribers", report)

    def test_no_drift_report(self):
        report = kd.render(self.LOCK, "c" * 40, None, reference(), "none", [])
        self.assertIn("- No drift.", report)

    def test_issue_body_is_clipped(self):
        body = kd.clip_for_issue("x" * (kd.MAX_ISSUE_BODY + 10), "https://run")
        self.assertLessEqual(len(body), kd.MAX_ISSUE_BODY)
        self.assertIn("https://run", body)
        self.assertEqual(kd.clip_for_issue("short", "u"), "short")


if __name__ == "__main__":
    unittest.main()
