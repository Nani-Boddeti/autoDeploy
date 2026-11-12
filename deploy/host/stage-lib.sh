# shellcheck shell=bash
# Shared by install-checkout.sh and install-app.sh (sourced, not executed). The caller
# defines die() and log().
#
# Trusted-binary staging: every input from BINDIR is first copied into a root-only
# temporary directory; validation, checksum verification and installation all use
# those COPIES, so a file that changes (or is swapped) in BINDIR after the copy
# cannot alter what was checked.
STAGE=""
declare -A STAGED=() # staged name -> source basename (key into SHA256SUMS)

stage_init() {
  STAGE="$(mktemp -d)"
  chmod 0700 "$STAGE"
  trap 'rm -rf -- "$STAGE"' EXIT
}

stage_copy() { # src name label
  [[ -f "$1" && ! -L "$1" ]] || die "$3: $1 is not a regular file"
  install -m 0600 -o root -g root "$1" "$STAGE/$2"
  STAGED["$2"]="$(basename -- "$1")"
}

# ELF header bytes only; the staged copy is never executed here.
elf_bytes() { od -An -v -tx1 -j"$2" -N"$3" "$1" | tr -d ' \n'; }
stage_require_elf() { # name label: ELF64 little-endian x86-64
  [[ "$(elf_bytes "$STAGE/$1" 0 5)" == 7f454c4602 && "$(elf_bytes "$STAGE/$1" 18 2)" == 3e00 ]] \
    || die "$2: ${STAGED[$1]} is not an ELF64 x86-64 executable"
}

# Verify every staged file against BINDIR/SHA256SUMS ("<sha256>  <basename>" lines, as
# written by sha256sum or build-static-git.sh). Without a SUMS file only a loud warning
# is printed. With one, EVERY staged file must be listed and match.
stage_verify() { # bindir
  local sums="$1/SHA256SUMS" name
  if [[ ! -e "$sums" ]]; then
    {
      echo "install: WARNING: $sums not found: the binaries are NOT integrity-checked."
      echo "install: WARNING: they run as root. Create it with: (cd $1 && sha256sum <files> > SHA256SUMS)"
    } >&2
    return 0
  fi
  [[ -f "$sums" && ! -L "$sums" ]] || die "$sums is not a regular file"
  install -m 0600 -o root -g root "$sums" "$STAGE/.sums"
  : >"$STAGE/.sums.checked"
  for name in "${!STAGED[@]}"; do
    awk -v n="${STAGED[$name]}" -v s="$name" '$2==n || $2=="*" n {print $1 "  " s; f=1} END {exit !f}' "$STAGE/.sums" \
      >>"$STAGE/.sums.checked" || die "$sums has no checksum for ${STAGED[$name]}"
  done
  (cd "$STAGE" && sha256sum --check --strict --quiet .sums.checked) || die "checksum mismatch against $sums"
  log "all ${#STAGED[@]} staged binaries match $sums"
}
