#!/bin/sh
# Makes the git fixtures of the watcher tests in $1.
# Step "base": a repo app with one commit, a bare remote, and the group folder.
# Step "work": a worktree .wt/app-7/app on branch app-7, two commits, a rebase on main, a merge into main,
# a push of main, and a reset of app-7.
set -eu
root=$1
step=$2
export GIT_AUTHOR_NAME=Dev GIT_AUTHOR_EMAIL=dev@example.com GIT_COMMITTER_NAME=Dev GIT_COMMITTER_EMAIL=dev@example.com
g() { git -c init.defaultBranch=main -c core.hooksPath=/dev/null "$@"; }
case $step in
base)
	g init -q --bare "$root/remote.git"
	g init -q "$root/app"
	cd "$root/app"
	echo one > a.txt
	g add a.txt
	g commit -q -m "Start"
	g remote add origin "$root/remote.git"
	g push -q -u origin main
	;;
work)
	cd "$root/app"
	mkdir -p "$root/.wt/app-7"
	g worktree add -q "$root/.wt/app-7/app" -b app-7
	cd "$root/.wt/app-7/app"
	printf 'two\nthree\n' > b.txt
	g add b.txt
	g commit -q -m "Add b" -m "Item: APP-7"
	echo four >> b.txt
	g commit -q -am "Grow b"
	cd "$root/app"
	echo main > c.txt
	g add c.txt
	g commit -q -m "Main moves"
	cd "$root/.wt/app-7/app"
	g rebase -q main
	cd "$root/app"
	g merge -q --ff-only app-7
	g push -q origin main
	cd "$root/.wt/app-7/app"
	g reset -q --hard HEAD~1
	;;
esac
