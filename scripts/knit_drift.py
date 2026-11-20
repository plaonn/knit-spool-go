#!/usr/bin/env python3
"""Report how far upstream Knit has moved past the pins in knit-reference.lock.

The check compares the pinned Knit Spool Protocol text with the current text and
the pinned Kotlin reference with upstream, then writes a Markdown report that a
maintainer triages. It never edits the pins: deciding whether a change needs
implementation work stays with a person.

Signals:
  * protocol version header and Section 13 test vectors
  * spool-side (S-) and shared (B-) normative rules: added, changed, removed,
    graded by RFC 2119 keyword; client-only (C-) rules are counted only
  * Appendix B change-log entries that are new or amended since the pin,
    with their "Asks of implementers" column
  * reference repository renames, commits past the pin and the latest release

Standard library only. Set GITHUB_TOKEN to raise the GitHub API rate limit.
"""

from __future__ import annotations

import argparse
import dataclasses
import difflib
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

API = "https://api.github.com"
LEVELS = ("none", "informational", "review", "must-review")
RULE_ROW = re.compile(r"^\|\s*\*\*(?P<id>[SBC]-\d+(?:\.\d+)*-\d+)\*\*\s*\|(?P<text>.*)\|\s*$")
CHANGELOG_ROW = re.compile(r"^\|\s*(?P<date>\d{4}-\d{2}-\d{2})\s*\|(?P<rest>.*)\|\s*$")
HEADER_ROW = re.compile(r"^\|\s*(?P<key>Protocol version|This revision)\s*\|\s*(?P<value>[^|]*?)\s*\|")
BOLD = re.compile(r"\*\*(.+?)\*\*")
MAX_RULE_TEXT = 500
MAX_ISSUE_BODY = 60000


# ---------------------------------------------------------------------------
# Pure analysis (unit tested)


def read_lock(path: Path) -> dict[str, str]:
    values: dict[str, str] = {}
    for number, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        key, sep, value = line.partition("=")
        if not sep or not key.strip() or not value.strip():
            raise ValueError(f"{path}:{number}: expected KEY=VALUE")
        values[key.strip()] = value.strip()
    required = ("SPEC_REPO", "SPEC_PATH", "SPEC_COMMIT", "SPEC_REVISION", "PROTOCOL_VERSION",
                "REFERENCE_REPO", "REFERENCE_COMMIT")
    missing = [key for key in required if key not in values]
    if missing:
        raise ValueError(f"{path}: missing {', '.join(missing)}")
    return values


def _squash(text: str) -> str:
    return re.sub(r"\s+", " ", text).strip()


def parse_header(spec: str) -> dict[str, str]:
    header: dict[str, str] = {}
    for line in spec.splitlines():
        match = HEADER_ROW.match(line)
        if match and match["key"] not in header:
            header[match["key"]] = match["value"].strip()
    return header


def parse_rules(spec: str) -> dict[str, str]:
    rules: dict[str, str] = {}
    for line in spec.splitlines():
        match = RULE_ROW.match(line)
        if match:
            rules[match["id"]] = _squash(match["text"])
    return rules


def section(spec: str, number: str) -> str:
    """Return the body of top-level section `## <number>.` up to the next `## ` heading."""
    lines = spec.splitlines()
    start = next((i for i, line in enumerate(lines) if line.startswith(f"## {number}.")), None)
    if start is None:
        return ""
    end = next((i for i in range(start + 1, len(lines)) if lines[i].startswith("## ")), len(lines))
    return "\n".join(lines[start + 1:end])


def parse_changelog(spec: str) -> list[dict[str, str]]:
    entries = []
    for line in section(spec, "Appendix B").splitlines():
        match = CHANGELOG_ROW.match(line)
        if not match:
            continue
        cells = [cell.strip() for cell in match["rest"].split("|")]
        change = cells[0] if cells else ""
        asks = cells[-1] if len(cells) > 1 else ""
        title = BOLD.search(change)
        entries.append({
            "date": match["date"],
            "title": _squash(title.group(1) if title else change[:200]),
            "asks": _squash(asks),
            "row": _squash(line),
        })
    return entries


def keyword_level(text: str) -> str:
    if re.search(r"\bMUST\b|\bSHALL\b|\bREQUIRED\b", text):
        return "MUST"
    if re.search(r"\bSHOULD\b|\bRECOMMENDED\b", text):
        return "SHOULD"
    if re.search(r"\bMAY\b|\bOPTIONAL\b", text):
        return "MAY"
    return "informative"


@dataclasses.dataclass
class RuleChange:
    rule: str
    kind: str  # added, changed, removed
    keyword: str
    old: str
    new: str
    spool_ask: str = ""  # "date: Spools column" of the latest change-log entry naming the rule


@dataclasses.dataclass
class SpecDiff:
    pinned_header: dict[str, str]
    current_header: dict[str, str]
    rule_changes: list[RuleChange]
    client_rule_changes: int
    vectors_removed_lines: int
    vectors_added_lines: int
    changelog: list[dict[str, str]]


def _rule_order(rule_id: str) -> tuple:
    prefix, _, rest = rule_id.partition("-")
    return tuple(int(part) for part in re.split(r"[.-]", rest)) + (prefix,)


RULE_REF = re.compile(r"\b([SBC])-(\d+(?:\.\d+)*)-(\d+)(?:\s+to\s+\1-\2-(\d+))?")
SPOOLS_ASK = re.compile(r"\*\*Spools:\*\*\s*(.*?)(?:\*\*Clients:\*\*|$)")


def spool_ask(asks: str) -> str:
    """The part of an "Asks of implementers" cell addressed to spools."""
    match = SPOOLS_ASK.search(asks)
    if match:
        return match.group(1).strip().rstrip(".").strip()
    if asks.lower().startswith("none for spools"):
        return "none"
    return asks


def rule_asks(changelog: list[dict[str, str]]) -> dict[str, str]:
    """Map each rule an entry's title names (ranges expanded) to that entry's spool ask."""
    asks: dict[str, str] = {}
    for entry in changelog:
        for prefix, clause, first, last in RULE_REF.findall(entry["title"]):
            for number in range(int(first), int(last or first) + 1):
                asks[f"{prefix}-{clause}-{number}"] = f"{entry['date']}: {spool_ask(entry['asks']) or 'not stated'}"
    return asks


def diff_spec(pinned: str, current: str) -> SpecDiff:
    old_rules, new_rules = parse_rules(pinned), parse_rules(current)
    current_changelog = parse_changelog(current)
    asks = rule_asks(sorted(current_changelog, key=lambda entry: entry["date"]))
    changes: list[RuleChange] = []
    client_changes = 0
    for rule in sorted(set(old_rules) | set(new_rules), key=_rule_order):
        old, new = old_rules.get(rule, ""), new_rules.get(rule, "")
        if old == new:
            continue
        if rule.startswith("C-"):
            client_changes += 1
            continue
        kind = "added" if not old else "removed" if not new else "changed"
        keyword = keyword_level(new or old)
        if kind == "changed" and keyword_level(old) == "MUST":
            keyword = "MUST"
        changes.append(RuleChange(rule, kind, keyword, old, new, asks.get(rule, "")))

    old_vectors = section(pinned, "13").splitlines()
    new_vectors = section(current, "13").splitlines()
    removed = added = 0
    for line in difflib.unified_diff(old_vectors, new_vectors, lineterm="", n=0):
        if line.startswith(("---", "+++", "@@")):
            continue
        if line.startswith("-") and line[1:].strip():
            removed += 1
        elif line.startswith("+") and line[1:].strip():
            added += 1

    pinned_rows = {entry["row"] for entry in parse_changelog(pinned)}
    changelog = [entry for entry in current_changelog if entry["row"] not in pinned_rows]

    return SpecDiff(parse_header(pinned), parse_header(current), changes, client_changes,
                    removed, added, changelog)


@dataclasses.dataclass
class ReferenceState:
    pinned_repo: str
    canonical_repo: str
    pinned_commit: str
    head_commit: str
    commits_ahead: int
    release_tag: str
    release_commit: str
    release_contains_pin: bool


def grade(spec_changed: bool, diff: SpecDiff | None, ref: ReferenceState) -> tuple[str, list[str]]:
    """Return the overall level and one reason per contributing signal."""
    reasons: list[tuple[str, str]] = []
    if diff is not None:
        old_version = diff.pinned_header.get("Protocol version")
        new_version = diff.current_header.get("Protocol version")
        if old_version != new_version:
            reasons.append(("must-review", f"protocol version changed: {old_version} -> {new_version}"))
        if diff.vectors_removed_lines:
            reasons.append(("must-review",
                            f"Section 13 test vectors: {diff.vectors_removed_lines} existing line(s) changed or removed"))
        musts = [c.rule for c in diff.rule_changes if c.keyword == "MUST"]
        if musts:
            reasons.append(("must-review", f"{len(musts)} spool/shared rule(s) with MUST text added, changed or removed"))
        removed = [c.rule for c in diff.rule_changes if c.kind == "removed" and c.keyword != "MUST"]
        if removed:
            reasons.append(("review", f"{len(removed)} spool/shared rule(s) removed"))
        shoulds = [c.rule for c in diff.rule_changes if c.keyword == "SHOULD" and c.kind != "removed"]
        if shoulds:
            reasons.append(("review", f"{len(shoulds)} spool/shared rule(s) with SHOULD text added or changed"))
        others = [c.rule for c in diff.rule_changes if c.keyword in ("MAY", "informative") and c.kind != "removed"]
        if others:
            reasons.append(("informational", f"{len(others)} optional or informative spool/shared rule(s) added or changed"))
        if diff.vectors_added_lines:
            reasons.append(("informational", f"Section 13 test vectors gained {diff.vectors_added_lines} line(s)"))
        if diff.client_rule_changes:
            reasons.append(("informational", f"{diff.client_rule_changes} client-only rule(s) changed"))
        if diff.changelog:
            reasons.append(("informational", f"{len(diff.changelog)} change-log entr(y/ies) new or amended since the pin"))
    if spec_changed and diff is not None and not reasons:
        reasons.append(("informational", "specification text changed outside rules, vectors and change log"))
    if ref.canonical_repo.lower() != ref.pinned_repo.lower():
        reasons.append(("review", f"reference repository is now {ref.canonical_repo} (pinned as {ref.pinned_repo})"))
    if ref.release_tag and ref.release_commit != ref.pinned_commit:
        if ref.release_contains_pin:
            reasons.append(("review", f"reference release {ref.release_tag} is newer than the pinned commit"))
        else:
            reasons.append(("review", f"reference release {ref.release_tag} does not contain the pinned commit"))
    if ref.commits_ahead:
        reasons.append(("informational", f"reference default branch is {ref.commits_ahead} commit(s) past the pin"))

    level = "none"
    for reason_level, _ in reasons:
        if LEVELS.index(reason_level) > LEVELS.index(level):
            level = reason_level
    return level, [f"**{lvl}**: {text}" for lvl, text in reasons]


def _clip(text: str) -> str:
    text = text.replace("\n", " ")
    return text if len(text) <= MAX_RULE_TEXT else text[:MAX_RULE_TEXT].rstrip() + " …"


def render(lock: dict[str, str], spec_head: str, diff: SpecDiff | None, ref: ReferenceState,
           level: str, reasons: list[str]) -> str:
    spec_repo, spec_path = lock["SPEC_REPO"], lock["SPEC_PATH"]
    out = [
        "# Upstream Knit drift report",
        "",
        f"Overall level: **{level}**",
        "",
        "Levels are keyword-based triage aids. Many MUST rules belong to optional features that a spool "
        "conforms to by not advertising them; read each entry's change-log *Spools* column before deciding.",
        "",
        "## Pins",
        "",
        "| | Pinned | Upstream now |",
        "| --- | --- | --- |",
        f"| Specification | [`{lock['SPEC_COMMIT'][:12]}`](https://github.com/{spec_repo}/blob/{lock['SPEC_COMMIT']}/{spec_path}) "
        f"revision `{lock['SPEC_REVISION']}` | [`{spec_head[:12]}`](https://github.com/{spec_repo}/blob/{spec_head}/{spec_path})"
        + (f" revision `{diff.current_header.get('This revision', '?')}`" if diff else "") + " |",
        f"| Reference repository | `{ref.pinned_repo}` | `{ref.canonical_repo}` |",
        f"| Reference commit | [`{ref.pinned_commit[:12]}`](https://github.com/{ref.canonical_repo}/commit/{ref.pinned_commit}) "
        f"| [`{ref.head_commit[:12]}`](https://github.com/{ref.canonical_repo}/compare/{ref.pinned_commit}...{ref.head_commit}) "
        f"({ref.commits_ahead} ahead) |",
        f"| Reference latest release | | `{ref.release_tag or 'none'}`"
        + (f" at `{ref.release_commit[:12]}`" if ref.release_commit else "") + " |",
        "",
        "## Signals",
        "",
    ]
    out += [f"- {reason}" for reason in reasons] or ["- No drift."]

    if diff and diff.rule_changes:
        out += ["", "## Spool and shared rule changes", ""]
        for change in diff.rule_changes:
            out.append(f"- **{change.rule}** {change.kind}, {change.keyword}")
            if change.kind == "changed":
                out.append(f"  - was: {_clip(change.old)}")
                out.append(f"  - now: {_clip(change.new)}")
            else:
                out.append(f"  - {_clip(change.new or change.old)}")
            out.append(f"  - change log asks spools: {_clip(change.spool_ask) if change.spool_ask else 'no entry names this rule'}")

    if diff and diff.changelog:
        out += ["", "## Change-log entries since the pin", "",
                "| Date | Change | Asks of implementers |", "| --- | --- | --- |"]
        for entry in diff.changelog:
            out.append(f"| {entry['date']} | {_clip(entry['title'])} | {_clip(entry['asks'])} |")

    out += [
        "",
        "## Next steps",
        "",
        "1. Triage each MUST and SHOULD entry against the implementation and record the decision.",
        "2. Check the `latest reference conformance` result below, when present.",
        "3. Land required changes, then update every value in `knit-reference.lock` and the README pin in one PR.",
        "   This report clears itself on the next run after the pins match upstream.",
    ]
    return "\n".join(out) + "\n"


def clip_for_issue(report: str, run_url: str) -> str:
    if len(report) <= MAX_ISSUE_BODY:
        return report
    note = f"\n\n… truncated. The full report is attached to the [workflow run]({run_url}).\n"
    return report[:MAX_ISSUE_BODY - len(note)] + note


# ---------------------------------------------------------------------------
# GitHub access


class GitHub:
    def __init__(self, token: str | None):
        self.token = token

    def _request(self, path: str, accept: str) -> bytes:
        request = urllib.request.Request(API + path, headers={
            "Accept": accept,
            "User-Agent": "knit-spool-go-drift-check",
            "X-GitHub-Api-Version": "2022-11-28",
        })
        if self.token:
            request.add_header("Authorization", f"Bearer {self.token}")
        with urllib.request.urlopen(request, timeout=60) as response:
            return response.read()

    def json(self, path: str):
        return json.loads(self._request(path, "application/vnd.github+json"))

    def raw(self, repo: str, path: str, ref: str) -> str:
        quoted = urllib.parse.quote(path)
        return self._request(f"/repos/{repo}/contents/{quoted}?ref={ref}", "application/vnd.github.raw").decode("utf-8")


def reference_state(gh: GitHub, lock: dict[str, str]) -> ReferenceState:
    pinned_repo, pin = lock["REFERENCE_REPO"], lock["REFERENCE_COMMIT"]
    meta = gh.json(f"/repos/{pinned_repo}")
    repo, branch = meta["full_name"], meta["default_branch"]
    compare = gh.json(f"/repos/{repo}/compare/{pin}...{branch}")
    head = compare["commits"][-1]["sha"] if compare.get("commits") else pin
    tag = release_commit = ""
    contains = True
    try:
        tag = gh.json(f"/repos/{repo}/releases/latest")["tag_name"]
    except urllib.error.HTTPError as error:
        if error.code != 404:
            raise
    if tag:
        release_commit = gh.json(f"/repos/{repo}/commits/{urllib.parse.quote(tag)}")["sha"]
        status = gh.json(f"/repos/{repo}/compare/{pin}...{release_commit}")["status"]
        contains = status in ("ahead", "identical")
    return ReferenceState(pinned_repo, repo, pin, head, int(compare.get("ahead_by", 0)), tag,
                          release_commit, contains)


def write_outputs(path: str | None, values: dict[str, str]) -> None:
    if not path:
        return
    with open(path, "a", encoding="utf-8") as handle:
        for key, value in values.items():
            handle.write(f"{key}={value}\n")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--lock", type=Path, default=Path(__file__).resolve().parent.parent / "knit-reference.lock")
    parser.add_argument("--report", type=Path, help="write the Markdown report here (default: stdout)")
    parser.add_argument("--issue-body", type=Path, help="also write a report clipped to GitHub's issue body limit")
    parser.add_argument("--run-url", default="", help="workflow run link used when the issue body is clipped")
    args = parser.parse_args(argv)

    lock = read_lock(args.lock)
    gh = GitHub(os.environ.get("GITHUB_TOKEN"))
    spec_repo, spec_path, spec_pin = lock["SPEC_REPO"], lock["SPEC_PATH"], lock["SPEC_COMMIT"]

    latest = gh.json(f"/repos/{spec_repo}/commits?path={urllib.parse.quote(spec_path)}&per_page=1")
    spec_head = latest[0]["sha"] if latest else spec_pin
    pinned_text = gh.raw(spec_repo, spec_path, spec_pin)
    pinned_header = parse_header(pinned_text)
    if pinned_header.get("This revision") != lock["SPEC_REVISION"] or \
            pinned_header.get("Protocol version") != lock["PROTOCOL_VERSION"]:
        print(f"knit-reference.lock disagrees with the pinned specification header: {pinned_header}", file=sys.stderr)
        return 2

    spec_changed = spec_head != spec_pin
    diff = None
    if spec_changed:
        current_text = gh.raw(spec_repo, spec_path, spec_head)
        if pinned_text == current_text:
            spec_changed = False
        else:
            diff = diff_spec(pinned_text, current_text)
    ref = reference_state(gh, lock)
    level, reasons = grade(spec_changed, diff, ref)
    report = render(lock, spec_head, diff, ref, level, reasons)

    if args.report:
        args.report.write_text(report, encoding="utf-8")
    else:
        sys.stdout.write(report)
    if args.issue_body:
        args.issue_body.write_text(clip_for_issue(report, args.run_url), encoding="utf-8")
    write_outputs(os.environ.get("GITHUB_OUTPUT"), {
        "drift": "true" if level != "none" else "false",
        "level": level,
        "reference_repo": ref.canonical_repo,
        "reference_release": ref.release_tag,
        "reference_release_commit": ref.release_commit,
        # Changes when something worth a notification changes; reference commits alone do not.
        "drift_key": f"spec:{spec_head[:12]} release:{ref.release_tag or '-'} level:{level}",
        "reference_release_newer": "true" if ref.release_commit and ref.release_commit != ref.pinned_commit else "false",
    })
    print(f"drift level: {level}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
