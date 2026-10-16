#!/usr/bin/env python3
"""Generate linked, categorized release notes from commits between Git tags."""

import argparse
import re
import subprocess
from pathlib import Path


CATEGORIES = (
    ("🚀 Features", {"feat"}),
    ("🐛 Bug Fixes", {"fix"}),
    ("🔒 Security", {"security"}),
    (
        "🧰 Maintenance, Infrastructure & Documentation",
        {"chore", "ci", "dependencies", "docs", "refactor", "style", "test"},
    ),
)
DEFAULT_CATEGORY = "📦 Other Changes"
CONVENTIONAL_COMMIT = re.compile(
    r"^(?P<type>[a-z][a-z0-9-]*)(?:\((?P<scope>[^)]*)\))?(?P<breaking>!)?:\s*(?P<subject>.+)$"
)
MARKDOWN_SPECIAL = re.compile(r"([\\`*_{}\[\]<>!|])")


def git(*arguments: str) -> str:
    result = subprocess.run(
        ["git", *arguments],
        check=True,
        text=True,
        capture_output=True,
    )
    return result.stdout


def previous_tag(current_tag: str) -> str | None:
    try:
        return git("describe", "--tags", "--abbrev=0", f"{current_tag}^").strip()
    except subprocess.CalledProcessError:
        return None


def category_for(commit_type: str, scope: str = "") -> str:
    if commit_type == "security" or scope.lower() == "security":
        return "🔒 Security"
    for category, commit_types in CATEGORIES:
        if commit_type in commit_types:
            return category
    return DEFAULT_CATEGORY


def escape_markdown(value: str) -> str:
    return MARKDOWN_SPECIAL.sub(r"\\\1", value)


def commit_entries(
    current_tag: str, previous: str | None, repository: str
) -> dict[str, list[str]]:
    revision_range = f"{previous}..{current_tag}" if previous else current_tag
    lines = git(
        "log",
        "--first-parent",
        "--no-merges",
        "--format=%H%x09%s",
        revision_range,
    ).splitlines()
    entries = {category: [] for category, _ in CATEGORIES}
    entries[DEFAULT_CATEGORY] = []

    for line in lines:
        sha, subject = line.split("\t", maxsplit=1)
        match = CONVENTIONAL_COMMIT.match(subject)
        commit_type = match.group("type") if match else ""
        scope = (match.group("scope") or "") if match else ""
        title = match.group("subject") if match else subject
        if match and match.group("breaking"):
            title = f"{title} (breaking change)"
        category = category_for(commit_type, scope)
        short_sha = sha[:7]
        commit_url = f"https://github.com/{repository}/commit/{sha}"
        entries[category].append(
            f"- {escape_markdown(title)} ([{short_sha}]({commit_url}))"
        )
    return entries


def render(current_tag: str, previous: str | None, repository: str) -> str:
    entries = commit_entries(current_tag, previous, repository)

    sections = [f"# {current_tag}"]
    if previous:
        compare_url = f"https://github.com/{repository}/compare/{previous}...{current_tag}"
        sections.append(f"Changes since [{previous}]({compare_url})")
    sections.append("")

    for category, _ in (*CATEGORIES, (DEFAULT_CATEGORY, set())):
        category_entries = entries[category]
        if not category_entries:
            continue
        sections.extend((f"## {category}", "", *category_entries, ""))

    if not any(entries.values()):
        sections.extend(("No categorized commits were found.", ""))
    return "\n".join(sections).rstrip() + "\n"


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--tag", required=True)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()

    previous = previous_tag(args.tag)
    args.output.write_text(
        render(args.tag, previous, args.repository), encoding="utf-8"
    )


if __name__ == "__main__":
    main()
