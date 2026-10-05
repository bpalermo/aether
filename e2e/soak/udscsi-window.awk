# The uds-csi step's window detector (#1243), used by churn.sh. Input: one line
# per observation of a plugin pod on the victim's node, oldest first, as
#   pod=<name> del=<deletionTimestamp or empty> ready=<True|False or empty>
# from a `kubectl get pods -w` stream (plus `get` polls if the watch died); the
# last line per pod is its current state. -v orig=<the node's plugin pod before
# the roll>. Prints ONE word:
#   unseen  orig not observed yet (the watch has not listed it)
#   up      orig is not terminating: the roll has not reached this node
#   down    orig is terminating or gone and no other pod on the node is Ready
#           and not terminating: the node's plugin is DOWN -- delete now
#   back    orig is terminating or gone and a replacement is already Ready:
#           the down window passed before the driver acted
# No early `exit` in the main rule (SIGPIPE rule, #1121).
{
	name = ""
	d = ""
	r = ""
	for (i = 1; i <= NF; i++) {
		if ($i ~ /^pod=/) name = substr($i, 5)
		else if ($i ~ /^del=/) d = substr($i, 5)
		else if ($i ~ /^ready=/) r = substr($i, 7)
	}
	if (name != "") {
		seen[name] = 1
		del[name] = d
		ready[name] = r
	}
}
END {
	if (!(orig in seen)) {
		print "unseen"
	} else if (del[orig] == "") {
		print "up"
	} else {
		state = "down"
		for (n in seen) {
			if (n != orig && del[n] == "" && ready[n] == "True") state = "back"
		}
		print state
	}
}
