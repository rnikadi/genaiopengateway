"""MkDocs hooks for the documentation site (mkdocs.yml).

Docs link to files outside docs/, such as ../CHANGELOG.md or
../deploy/compose/README.md, which work on GitHub but are not part of the
site. Rewrite those links to the file on GitHub so they work in both places.
"""
import posixpath
import re

REPO = "https://github.com/rnikadi/genaiopengateway/blob/main/"
LINK = re.compile(r"\]\((?!https?:|#|mailto:)([^)\s]+)\)")


def on_page_markdown(markdown, page, config, files):
    base = posixpath.dirname(page.file.src_uri)

    def fix(match):
        target = match.group(1)
        path, _, anchor = target.partition("#")
        resolved = posixpath.normpath(posixpath.join("docs", base, path))
        if resolved.startswith("docs/") or resolved == "docs":
            return match.group(0)
        return "](" + REPO + resolved + ("#" + anchor if anchor else "") + ")"

    return LINK.sub(fix, markdown)
