# REPOS

Local clones. Nothing in here is tracked except this README.
Each clone keeps its own .git and is invisible to the parent repo -- no submodules.

Examples:

    REPOS/<github-owner>/<repo>        canonical working copy
    REPOS/<owner>_<yyyymmdd>/<repo>    dated side copy
    REPOS/temp_<purpose>/<repo>        scratch copy for one effort

First level is either an exact GitHub owner (canonical) or anything else (scratch,
delete when done). Canonical paths are derivable from the GitHub identifier, so a
reference to `owner/repo` resolves here on any machine.
