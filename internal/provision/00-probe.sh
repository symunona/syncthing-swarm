#!/usr/bin/env bash
# 00-probe.sh — read-only survey of a candidate swarm node.
#
# Piped over ssh (`ssh dest bash -s < 00-probe.sh`): never copied to the box,
# leaves nothing behind. Runs as the plain login user — NO SUDO, and every
# command here is a read. It cannot change the box.
#
# Emits NDJSON, one line per check, flushed as each check finishes, so the
# caller can render partial results instead of blocking on a final blob:
#
#   {"check":"box","state":"ok","ms":12,"data":{...}}
#   {"check":"stfolders","state":"skip","note":"no external drive mounted"}
#   {"check":"disks","state":"err","note":"lsblk: not found"}
#
# Env:
#   PROBE_BENCH=hash    run the sha256 throughput benchmark (default: off)
#   PROBE_SCAN_ROOTS    override the dirs scanned for .stfolder (space separated)
#
# No deep filesystem walks. The .stfolder scan is depth-capped and prunes on
# hit; file counts and sizes come from df, not from walking the tree.

export LC_ALL=C
PATH=$PATH:/usr/sbin:/sbin

# --- json emit ---------------------------------------------------------------

# jesc escapes a string for a JSON value (no surrounding quotes). Tabs matter:
# /etc/fstab is tab-separated, and a raw tab inside a JSON string is a control
# character — invalid JSON, which is how this bit us the first time.
jesc() {
	printf '%s' "$1" \
		| sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/\t/\\t/g' \
		| tr -d '\r' \
		| tr -d '\000-\010\013\014\016-\037\177' \
		| awk 'BEGIN{ORS=""} {print sep $0; sep="\\n"}'
}

# jstr emits a quoted JSON string.
jstr() { printf '"%s"' "$(jesc "$1")"; }

# now_ms — bash5 has EPOCHREALTIME; fall back to date.
if [ -n "${EPOCHREALTIME:-}" ]; then
	now_ms() { local t=${EPOCHREALTIME/,/.}; printf '%s' "$(( ${t%.*} * 1000 + 10#${t#*.} / 1000 ))"; }
else
	now_ms() { printf '%s' "$(( $(date +%s%N) / 1000000 ))"; }
fi

CHECK_T0=0
start() { CHECK_T0=$(now_ms); }
elapsed() { printf '%s' "$(( $(now_ms) - CHECK_T0 ))"; }

# ok <check> <raw-json-data>
ok() { printf '{"check":%s,"state":"ok","ms":%s,"data":%s}\n' "$(jstr "$1")" "$(elapsed)" "$2"; }
# skip <check> <note>   — check does not apply to this box (not an error)
skip() { printf '{"check":%s,"state":"skip","ms":%s,"note":%s}\n' "$(jstr "$1")" "$(elapsed)" "$(jstr "$2")"; }
# err <check> <note>    — check applies but failed; caller degrades gracefully
err() { printf '{"check":%s,"state":"err","ms":%s,"note":%s}\n' "$(jstr "$1")" "$(elapsed)" "$(jstr "$2")"; }
# note <check> <hint> <text> — "about to run something slow"
running() { printf '{"check":%s,"state":"start","hint":%s,"note":%s}\n' "$(jstr "$1")" "$(jstr "$2")" "$(jstr "$3")"; }

have() { command -v "$1" >/dev/null 2>&1; }

# oneline folds a pretty-printed JSON blob (lsblk/findmnt -J) onto a single line
# so it survives NDJSON framing. Only newlines go — indentation inside string
# values is left alone, so device model names keep their spacing.
oneline() { tr '\n' ' '; }

# first line of a file, empty if missing
slurp() { [ -r "$1" ] && tr -d '\000' <"$1" | head -n1 || printf ''; }

# --- box ---------------------------------------------------------------------

check_box() {
	start
	local model arch cores mem_kb swap_kb host user
	# rpi / most ARM SBCs advertise themselves here; x86 does not
	model=$(slurp /proc/device-tree/model)
	[ -z "$model" ] && model=$(slurp /sys/class/dmi/id/product_name)
	[ -z "$model" ] && model="unknown"

	# one subshell, not two: on an ARM11 core each fork costs real milliseconds
	PRETTY_NAME=""; VERSION_CODENAME=""
	[ -r /etc/os-release ] && . /etc/os-release 2>/dev/null

	arch=$(uname -m)
	cores=$(nproc 2>/dev/null || printf '1')
	# MemTotal and SwapTotal in one pass. Swap matters on a 512MB Pi: syncthing's
	# hashing working set can outgrow RAM on a box this small.
	eval "$(awk '/^MemTotal:/{printf "mem_kb=%s ", $2} /^SwapTotal:/{printf "swap_kb=%s", $2}' /proc/meminfo 2>/dev/null)"
	host=$(hostname 2>/dev/null)
	user=$(id -un)

	printf -v _d '{"model":%s,"os":%s,"codename":%s,"arch":%s,"kernel":%s,"cores":%s,"memBytes":%s,"swapBytes":%s,"hostname":%s,"user":%s,"uid":%s}' \
		"$(jstr "$model")" "$(jstr "$PRETTY_NAME")" "$(jstr "$VERSION_CODENAME")" "$(jstr "$arch")" \
		"$(jstr "$(uname -r)")" "${cores:-1}" "$(( ${mem_kb:-0} * 1024 ))" "$(( ${swap_kb:-0} * 1024 ))" \
		"$(jstr "$host")" "$(jstr "$user")" "$(id -u)"
	ok box "$_d"
}

# --- inotify -----------------------------------------------------------------
# The one that bites: default max_user_watches=8192 is far below what syncthing
# needs (roughly one watch per directory). Over the limit, syncthing silently
# stops watching and falls back to periodic scans.

check_inotify() {
	start
	local w i
	w=$(slurp /proc/sys/fs/inotify/max_user_watches)
	i=$(slurp /proc/sys/fs/inotify/max_user_instances)
	printf -v _d '{"maxUserWatches":%s,"maxUserInstances":%s}' "${w:-0}" "${i:-0}"
	ok inotify "$_d"
}

# --- disks -------------------------------------------------------------------
# One lsblk call answers: external? HDD or SSD? where mounted? Raw JSON goes to
# the caller, which owns the parent/child logic (TRAN lives on the disk, the
# mountpoint on its partition).

check_disks() {
	start
	have lsblk || { err disks "lsblk not found"; return; }
	local out
	# -e 7 drops loop devices (snaps): pure noise, and on a laptop they outnumber
	# the real disks 20:1.
	out=$(lsblk -J -b -e 7 -o NAME,PATH,TYPE,SIZE,ROTA,TRAN,HOTPLUG,FSTYPE,UUID,LABEL,MOUNTPOINT,MODEL 2>/dev/null | oneline)
	[ -z "$out" ] && { err disks "lsblk produced no output"; return; }
	# lsblk -J already prints {"blockdevices":[...]}
	ok disks "$out"
}

check_mounts() {
	start
	have findmnt || { err mounts "findmnt not found"; return; }
	local out
	out=$(findmnt -J -b -o TARGET,SOURCE,FSTYPE,OPTIONS,SIZE,USED,AVAIL 2>/dev/null | oneline)
	[ -z "$out" ] && { err mounts "findmnt produced no output"; return; }
	ok mounts "$out"
}

check_fstab() {
	start
	[ -r /etc/fstab ] || { err fstab "/etc/fstab unreadable"; return; }
	local lines sep=""
	lines=""
	while IFS= read -r line; do
		case "$line" in ''|\#*) continue ;; esac
		lines="$lines$sep$(jstr "$line")"
		sep=","
	done </etc/fstab
	ok fstab "[$lines]"
}

# --- security ----------------------------------------------------------------
# Deliberately small. We report; we never edit the ssh access path.

# svc_state prints the unit's enablement as a JSON string: enabled | disabled |
# not-found.
#
# This reads the symlink farm directly instead of shelling out to
# `systemctl is-enabled`. "Enabled" IS a .wants/ symlink, so the answer is the
# same — but systemctl costs ~300ms per call on an ARM11 core, and we ask about
# five units. That was 1.5s of the probe, spent on forks.
svc_state() {
	local unit=$1 t tmpl
	# a .wants/ symlink anywhere means enabled
	for t in /etc/systemd/system/*.wants/"$unit"; do
		[ -e "$t" ] && { jstr enabled; return; }
	done
	# installed but not enabled? templated units (syncthing@bob.service) live on
	# disk under their template name (syncthing@.service)
	tmpl=$unit
	case "$unit" in
		*@*.service) tmpl="${unit%%@*}@.service" ;;
	esac
	for t in "/etc/systemd/system/$tmpl" "/lib/systemd/system/$tmpl" "/usr/lib/systemd/system/$tmpl"; do
		[ -e "$t" ] && { jstr disabled; return; }
	done
	jstr not-found
}

# ACTIVE maps unit -> active state, filled by one systemctl call.
#
# "enabled" is a symlink and can be read for free; "active" cannot — and the
# difference matters. fail2ban installed, enabled, and then CRASHED on startup
# (bookworm ships no rsyslog, so its sshd jail found no /var/log/auth.log), and a
# probe that only read the symlink cheerfully called it done. Installed is not
# running.
#
# One `systemctl is-active u1 u2 …` prints one line per unit, in order, even for
# units that do not exist — so this costs a single fork, not five.
declare -A ACTIVE
init_active() {
	have systemctl || return 0
	local units out u
	units="ufw.service fail2ban.service unattended-upgrades.service hd-idle.service syncthing@$(id -un).service"
	out=$(systemctl is-active $units 2>/dev/null)
	set -- $units
	while IFS= read -r line; do
		[ -z "${1:-}" ] && break
		ACTIVE["$1"]=$line
		shift
	done <<-EOF
		$out
	EOF
}

svc_active() {
	printf '%s' "${ACTIVE[$1]:-unknown}"
}

tool_json() {
	# {"present":bool,"enabled":"..","active":".."}
	local bin=$1 unit=$2 present=false
	have "$bin" && present=true
	printf '{"present":%s,"enabled":%s,"active":%s}' \
		"$present" "$(svc_state "$unit")" "$(jstr "$(svc_active "$unit")")"
}

# ufw_json is tool_json plus firewallUp: ufw's OWN enabled flag, read
# straight from /etc/ufw/ufw.conf rather than inferred from systemd.
#
# The systemd unit (ufw.service) is Type=oneshot + RemainAfterExit=yes, and
# apt's postinst enables AND starts it the moment the package lands —
# regardless of whether anyone ever ran `ufw enable`. So present/enabled/
# active can all read "yes, installed and running" on a box whose firewall
# is not actually filtering anything. ENABLED=yes in ufw.conf is ufw's own
# record of whether `ufw enable` (or `ufw disable`) was the last thing run,
# and the file is 0644 — readable here with no sudo, same as everything
# else in this probe.
ufw_json() {
	local present=false firewall_up=false
	have ufw && present=true
	grep -q '^ENABLED=yes' /etc/ufw/ufw.conf 2>/dev/null && firewall_up=true
	printf '{"present":%s,"enabled":%s,"active":%s,"firewallUp":%s}' \
		"$present" "$(svc_state ufw.service)" "$(jstr "$(svc_active ufw.service)")" "$firewall_up"
}

check_security() {
	start
	local ufw f2b unatt listen sshd_port sshd_pw sshd_root

	ufw=$(ufw_json)
	f2b=$(tool_json fail2ban-client fail2ban.service)
	unatt=$(tool_json unattended-upgrade unattended-upgrades.service)

	# listening TCP sockets. Without root we get addresses but not other users'
	# process names — addresses are what the finding is about anyway
	# (0.0.0.0:x is exposed, 127.0.0.1:x is not).
	listen="[]"
	if have ss; then
		listen=$(ss -ltnH 2>/dev/null | awk '
			{ split($4, a, /:/); port = a[length(a)];
			  addr = substr($4, 1, length($4) - length(port) - 1);
			  printf "%s{\"addr\":\"%s\",\"port\":%s}", sep, addr, port; sep="," }
			END { }' )
		listen="[$listen]"
	fi

	# sshd_config is world-readable; no sudo needed. Last match wins in sshd, but
	# for our purposes (is password auth on at all?) we take the last explicit one.
	sshd_port=$(grep -rhiE '^[[:space:]]*Port[[:space:]]+' /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf 2>/dev/null | awk '{print $2}' | tail -n1)
	sshd_pw=$(grep -rhiE '^[[:space:]]*PasswordAuthentication[[:space:]]+' /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf 2>/dev/null | awk '{print tolower($2)}' | tail -n1)
	sshd_root=$(grep -rhiE '^[[:space:]]*PermitRootLogin[[:space:]]+' /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf 2>/dev/null | awk '{print tolower($2)}' | tail -n1)
	# sshd defaults when unset
	[ -z "$sshd_port" ] && sshd_port=22
	[ -z "$sshd_pw" ] && sshd_pw=yes
	[ -z "$sshd_root" ] && sshd_root=prohibit-password

	printf -v _d '{"ufw":%s,"fail2ban":%s,"unattendedUpgrades":%s,"listening":%s,"sshdPort":%s,"passwordAuth":%s,"permitRootLogin":%s}' \
		"$ufw" "$f2b" "$unatt" "$listen" "${sshd_port:-22}" "$(jstr "$sshd_pw")" "$(jstr "$sshd_root")"
	ok security "$_d"
}

# --- spindown ----------------------------------------------------------------
# hd-idle, not hdparm -S: most USB-SATA bridges silently swallow the drive's own
# idle timer, so you set it, hdparm says OK, and the disk never sleeps. hd-idle
# watches /proc/diskstats and issues the sleep itself.
# (Drive power state via `hdparm -C` needs root — the probe stays sudo-free, so
# we report the daemon, not the current state.)

check_spindown() {
	start
	local present=false conf=""
	have hd-idle && present=true
	[ -r /etc/default/hd-idle ] && conf=$(grep -vE '^[[:space:]]*(#|$)' /etc/default/hd-idle 2>/dev/null | tr '\n' ' ')
	printf -v _d '{"present":%s,"enabled":%s,"active":%s,"config":%s}' \
		"$present" "$(svc_state hd-idle.service)" "$(jstr "$(svc_active hd-idle.service)")" "$(jstr "$conf")"
	ok spindown "$_d"
}

# --- power -------------------------------------------------------------------
# The Pi records under-voltage in hardware, and this is the check that would have
# caught what killed rue: a bus-powered 2.5" HDD spikes ~1A on spin-up, while a
# Pi 1 B+ limits its whole USB bus to 600mA unless config.txt says
# max_usb_current=1. The board browns out, the SD card is corrupted mid-write,
# and the box never boots again. Enabling hd-idle turns one spin-up at boot into
# a spin-up every time the disk is touched — so the wizard must know whether this
# board can actually feed its disk before it starts cycling it.
#
# vcgencmd get_throttled bit 0 = under-voltage NOW, bit 16 = under-voltage has
# occurred since boot. Costs one fork and is read-only.

check_power() {
	start
	have vcgencmd || { skip power "not a Raspberry Pi (no vcgencmd)"; return; }
	local raw hex usb_max
	raw=$(vcgencmd get_throttled 2>/dev/null)   # throttled=0x50005
	hex=${raw#*=}
	[ -z "$hex" ] && { err power "vcgencmd gave no reading"; return; }
	# config.txt lives on the boot partition; readable without sudo
	usb_max=""
	for c in /boot/firmware/config.txt /boot/config.txt; do
		[ -r "$c" ] && { usb_max=$(grep -E '^[[:space:]]*max_usb_current' "$c" 2>/dev/null | head -n1); break; }
	done
	printf -v _d '{"throttled":%s,"maxUsbCurrent":%s}' "$(jstr "$hex")" "$(jstr "$usb_max")"
	ok power "$_d"
}

# --- syncthing / tailscale ---------------------------------------------------

check_syncthing() {
	start
	local present=false ver="" cfg="" user
	user=$(id -un)
	if have syncthing; then
		present=true
		ver=$(syncthing --version 2>/dev/null | head -n1)
	fi
	for d in "$HOME/.local/state/syncthing" "$HOME/.config/syncthing"; do
		[ -f "$d/config.xml" ] && { cfg=$d; break; }
	done
	printf -v _d '{"present":%s,"version":%s,"configDir":%s,"unit":%s,"enabled":%s,"active":%s}' \
		"$present" "$(jstr "$ver")" "$(jstr "$cfg")" \
		"$(jstr "syncthing@$user.service")" "$(svc_state "syncthing@$user.service")" \
		"$(jstr "$(svc_active "syncthing@$user.service")")"
	ok syncthing "$_d"
}

check_tailscale() {
	start
	have tailscale || { err tailscale "tailscale not installed — the node must be on the tailnet"; return; }
	# `tailscale ip -4` alone. `tailscale status` costs ~2s on an ARM11 core and
	# tells us nothing we act on: a tailnet IP already proves tailscaled is up.
	local ip4
	ip4=$(tailscale ip -4 2>/dev/null | head -n1)
	[ -z "$ip4" ] && { err tailscale "tailscaled not up (no tailnet IP)"; return; }
	printf -v _d '{"ip4":%s}' "$(jstr "$ip4")"
	ok tailscale "$_d"
}

# --- .stfolder scan ----------------------------------------------------------
# Bounded on purpose: only the external drive's mountpoint, depth <= 3, prune on
# hit. A .stfolder at depth 3 means the folder root sits at depth 2, which covers
# every realistic layout (/media/hdd/syncthing/dropx). Pruning the marker stops
# descent into folder contents. Result: dozens of stat()s, not millions.
#
# The folder ID is NOT on disk. .stfolder is a bare marker whose only job is to
# prove the drive is mounted; syncthing keeps the ID in its index database, which
# is exactly what dies with the SD card. So the ID comes from the swarm (every
# other node's config carries it), joined to what is here by DIRECTORY NAME:
# sharing.Share creates folders at <root>/<label>, so on a drive our tooling
# populated, the directory name IS the label.
#
# We still read syncthing-folder.txt if it happens to exist, so that if upstream
# ever starts stamping the ID into the marker, this picks it up for free.

scan_roots() {
	if [ -n "${PROBE_SCAN_ROOTS:-}" ]; then
		printf '%s\n' $PROBE_SCAN_ROOTS
		return
	fi
	# real filesystems mounted somewhere that is not the OS itself
	findmnt -rno TARGET,FSTYPE 2>/dev/null | while read -r target fstype; do
		case "$fstype" in
			ext2|ext3|ext4|xfs|btrfs|f2fs|exfat|ntfs|ntfs3|vfat|zfs) ;;
			*) continue ;;
		esac
		case "$target" in
			/|/boot|/boot/*|/var|/var/*|/home|/usr|/usr/*|/snap/*) continue ;;
		esac
		printf '%s\n' "$target"
	done
}

check_stfolders() {
	start
	local roots
	roots=$(scan_roots)
	[ -z "$roots" ] && { skip stfolders "no non-OS filesystem mounted (external drive absent or not mounted)"; return; }

	local out sep="" root marker dir id bytes iused

	# Size the FOLDERS, not the filesystem.
	#
	# The scan ETA was originally derived from `df` used-bytes, to dodge an
	# expensive du. That was wrong by 15x on the first real box: rue's drive holds
	# 305 GiB, but only 20 GiB of it is syncthing folders — syncthing hashes what
	# you configure, not what the disk contains. An ETA that says "7 hours" when
	# the truth is "13 minutes" is an ETA nobody reads.
	#
	# du is only affordable when the tree is small, so gate it on the inode count
	# (which df gives us for free). rue: 31k inodes, du took ~1s.
	iused=$(df --output=iused "$(printf '%s\n' $roots | head -n1)" 2>/dev/null | tail -n1 | tr -d ' ')
	local do_du=1
	[ "${iused:-0}" -gt "${PROBE_DU_MAX_INODES:-400000}" ] && do_du=0

	out=""
	for root in $roots; do
		while IFS= read -r marker; do
			[ -z "$marker" ] && continue
			dir=$(dirname "$marker")
			id=""
			# read the id if a future syncthing ever stamps one here (today: never)
			[ -r "$marker/syncthing-folder.txt" ] && \
				id=$(grep -iE '^[[:space:]]*folderID:' "$marker/syncthing-folder.txt" 2>/dev/null | head -n1 | sed -E 's/^[^:]*:[[:space:]]*//' | tr -d '\r')
			bytes=0
			[ "$do_du" = 1 ] && bytes=$(du -sb "$dir" 2>/dev/null | cut -f1)
			out="$out$sep{\"path\":$(jstr "$dir"),\"root\":$(jstr "$root"),\"id\":$(jstr "$id"),\"name\":$(jstr "$(basename "$dir")"),\"bytes\":${bytes:-0}}"
			sep=","
		done <<-EOF
			$(find "$root" -mindepth 1 -maxdepth 3 -type d \
				\( -name lost+found -o -name .stversions -o -name '.Trash-*' -o -name '.@__thumb' \) -prune -o \
				-type d -name .stfolder -print -prune 2>/dev/null)
		EOF
	done
	ok stfolders "[$out]"
}

# --- capacity ----------------------------------------------------------------
# df, not du. Inodes-used is an upper bound on inotify watches needed; used-bytes
# is the input to the initial-scan ETA. Both are instant. Walking the tree for
# exact numbers would cost hours on a USB2 HDD and buy nothing.

check_capacity() {
	start
	local roots out sep="" root
	roots=$(scan_roots)
	[ -z "$roots" ] && { skip capacity "no external filesystem to measure"; return; }
	out=""
	for root in $roots; do
		local size used avail inodes iused
		read -r size used avail <<-EOF
			$(df -B1 --output=size,used,avail "$root" 2>/dev/null | tail -n1)
		EOF
		# NB: `df -i --output=...` is an error ("mutually exclusive"). The inode
		# fields are selected through --output alone, with no -i.
		read -r inodes iused <<-EOF
			$(df --output=itotal,iused "$root" 2>/dev/null | tail -n1)
		EOF
		out="$out$sep{\"root\":$(jstr "$root"),\"sizeBytes\":${size:-0},\"usedBytes\":${used:-0},\"availBytes\":${avail:-0},\"inodesTotal\":${inodes:-0},\"inodesUsed\":${iused:-0}}"
		sep=","
	done
	ok capacity "[$out]"
}

# --- hash benchmark ----------------------------------------------------------
# The initial scan is hash-bound, not I/O-bound, on a small ARM core: this single
# number predicts the whole adoption ETA. ~2s, no disk touched, nothing installed.

check_hash() {
	[ "${PROBE_BENCH:-}" = "hash" ] || return 0
	running hash "~2s" "sha256 throughput — predicts the initial scan ETA"
	start
	# 16 MiB, not 64: on rue (11.9 MiB/s) 64 MiB cost 5.5s for a number that is
	# just as good from 16. Still milliseconds of resolution on a fast box.
	local mb=16 t0 t1 ms bps
	have sha256sum || { err hash "sha256sum not found"; return; }
	t0=$(now_ms)
	dd if=/dev/zero bs=1M count=$mb 2>/dev/null | sha256sum >/dev/null 2>&1
	t1=$(now_ms)
	ms=$(( t1 - t0 ))
	[ "$ms" -le 0 ] && ms=1
	bps=$(( mb * 1048576 * 1000 / ms ))
	printf -v _d '{"bytesPerSec":%s,"sampleBytes":%s}' "$bps" "$(( mb * 1048576 ))"
	ok hash "$_d"
}

# --- run ---------------------------------------------------------------------

init_active

check_box
check_inotify
check_disks
check_mounts
check_fstab
check_security
check_spindown
check_power
check_syncthing
check_tailscale
check_capacity
check_stfolders
check_hash
