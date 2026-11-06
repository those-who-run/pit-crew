# What `make check` and `make hooks` need, checked before either starts (the
# Makefile's `prereqs`): Go at go.mod's toolchain version, or a go command allowed
# to fetch it (GOTOOLCHAIN); gofmt; the C compiler cgo uses for the race detector;
# python3; pre-commit; git; and make. It prints one line for each tool that is
# missing or can't be used, saying how to fix it, and fails if there is any.
#
#     sh scripts/prereqs.sh
#
# Run from the module's directory. It installs nothing and needs no network: past
# the shell's builtins it runs only `go env`, once, outside the module so the go
# command doesn't switch toolchains for it. POSIX sh, so it runs whatever else is
# missing. tests/test_prereqs.py runs it against fake tools.

status=0

missing() {
	printf '%s\n' "$*" >&2
	status=1
}

have() {
	command -v "$1" >/dev/null 2>&1
}

# A Go version as three numbers, "1 27 3" for go1.27.3: a release candidate or
# beta (go1.28rc1) sorts before its .0 release, and anything after the version
# (go1.26.10-X:nodwarf5, as some distributions build it) is dropped.
numbers() {
	v=${1#go}
	v=${v%%[!0-9.a-z]*}
	pre=
	case $v in *[a-z]*) pre=1 v=${v%%[a-z]*} ;; esac
	major=${v%%.*}
	rest=${v#"$major"}
	rest=${rest#.}
	minor=${rest%%.*}
	patch=${rest#"$minor"}
	patch=${patch#.}
	[ -n "$pre" ] && patch=-1
	echo "${major:-0} ${minor:-0} ${patch:-0}"
}

# Whether Go version $1 is older than $2.
older() {
	# shellcheck disable=SC2046 # the split into three numbers is the point
	set -- $(numbers "$1") $(numbers "$2")
	[ "$1" -ne "$4" ] && { [ "$1" -lt "$4" ]; return; }
	[ "$2" -ne "$5" ] && { [ "$2" -lt "$5" ]; return; }
	[ "$3" -lt "$6" ]
}

# The Go version the module needs: go.mod's toolchain line, or its go line.
gomod_go=
gomod_toolchain=
if [ -f go.mod ]; then
	while read -r key value _ || [ -n "$key" ]; do
		case $key in
		go) gomod_go=go$value ;;
		toolchain) gomod_toolchain=$value ;;
		esac
	done <go.mod
fi
want=${gomod_toolchain:-$gomod_go}
if [ -n "$want" ]; then
	install_go="install $want from https://go.dev/dl/"
else
	install_go="install Go from https://go.dev/dl/ (go.mod names no version)"
fi

if ! have go; then
	missing "go: not found; $install_go and put its bin directory on PATH"
# Only standard output is parsed: the go command may warn on standard error first
# (GOPATH set to GOROOT, say), and that would shift every field.
elif ! env=$(cd / && GOTOOLCHAIN=local go env GOVERSION GOENV GOROOT CC 2>/dev/null); then
	err=$(cd / && GOTOOLCHAIN=local go env GOVERSION GOENV GOROOT CC 2>&1 >/dev/null)
	missing "go: \`go env\` failed ($err); $install_go"
else
	{
		read -r goversion
		read -r goenv
		read -r goroot
		read -r cc _
	} <<EOF
$env
EOF
	# GOTOOLCHAIN as the go command takes it: the environment, then `go env -w`'s
	# file, then the Go installation's own go.env (where a distribution may set
	# `local`), then the default, auto. Set but empty counts as unset, as it does
	# for the go command. The files are read either way: unsetting the
	# environment's value only helps where they don't say local too.
	toolchain= from=
	for file in "$goenv" "$goroot/go.env"; do
		[ -f "$file" ] || continue
		while IFS= read -r line || [ -n "$line" ]; do
			case $line in GOTOOLCHAIN=*) toolchain=${line#GOTOOLCHAIN=} ;; esac
		done <"$file"
		if [ -n "$toolchain" ]; then
			from="set in $file"
			break
		fi
	done
	toolchain=${toolchain:-auto}
	if [ -n "${GOTOOLCHAIN:-}" ]; then
		case $toolchain in
		auto | *+auto) fix="unset GOTOOLCHAIN or set it to auto" ;;
		*) fix="set GOTOOLCHAIN to auto (unsetting it leaves GOTOOLCHAIN=$toolchain, $from)" ;;
		esac
		toolchain=$GOTOOLCHAIN from="set in the environment"
	else
		fix="run \`go env -w GOTOOLCHAIN=auto\`"
	fi
	problem="go: $goversion is older than $want (go.mod) and GOTOOLCHAIN=$toolchain ($from)"
	case $toolchain in
	auto | *+auto) ;; # the go command fetches $want the first time it needs it
	path | *+path)
		if older "$goversion" "$want" && ! have "$want"; then
			missing "$problem looks on PATH for $want, which isn't there; $fix, or $install_go"
		fi
		;;
	local)
		older "$goversion" "$want" &&
			missing "$problem stops the go command fetching it; $fix, or $install_go"
		;;
	go[0-9]*)
		# The go command runs exactly that version, whatever is installed.
		older "$toolchain" "$want" &&
			missing "go: GOTOOLCHAIN=$toolchain ($from) is older than $want (go.mod); $fix, or set it to $want"
		;;
	esac
	have gofmt ||
		missing "gofmt: not found; it comes with Go, in $goroot/bin: put that on PATH"
	if [ -n "$cc" ] && ! have "$cc"; then
		missing "$cc: not found; the race detector (make check's go test -race) needs cgo's C compiler: install gcc, or another and set CC"
	fi
fi

py=${PY:-python3}
have "$py" ||
	missing "$py: not found; install Python 3 from https://www.python.org/downloads/ or the system's packages"
have pre-commit ||
	missing "pre-commit: not found; install it with \`pipx install pre-commit\` or \`$py -m pip install --user pre-commit\` (https://pre-commit.com/#install)"
have git ||
	missing "git: not found; install it from https://git-scm.com/downloads or the system's packages"
have make ||
	missing "make: not found; install GNU make from the system's packages"

if [ "$status" -eq 0 ]; then
	echo "prereqs: every tool make check and make hooks need is here"
fi
exit "$status"
