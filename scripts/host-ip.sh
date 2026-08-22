#!/bin/sh
# Print this host's primary LAN address, or fail loudly.
#
# The address has to be resolved here, on the host, because it is the only place
# the answer is correct: inside a container the interfaces are the container's
# (a 172.x bridge address), never the address another machine would connect to.
#
# Only used to put the right address in a self-signed certificate's SAN. The
# gateway emits relative URLs, so nothing else needs to know where it is.
set -eu

# Derive the interface from the default route rather than assuming a name.
# "en0" is right on a laptop and wrong on a Mac with Ethernet, on any host with
# a VPN (utun*, Tailscale), and on most Linux servers.
case "$(uname -s)" in
Darwin)
	iface=$(route -n get default 2>/dev/null | awk '/interface:/{print $2}')
	[ -n "${iface}" ] && ip=$(ipconfig getifaddr "${iface}" 2>/dev/null) || ip=""
	;;
Linux)
	ip=$(ip route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')
	;;
*)
	ip=""
	;;
esac

if [ -z "${ip:-}" ]; then
	# Guessing wrong surfaces as a certificate name mismatch or a player that
	# cannot reach the gateway — neither of which points back here. Say so
	# instead, and name the way out.
	echo "host-ip: could not determine this host's address on $(uname -s)." >&2
	echo "host-ip: set it explicitly, e.g. HOST_IP=192.0.2.10 make up-tls" >&2
	exit 1
fi

printf '%s\n' "${ip}"
