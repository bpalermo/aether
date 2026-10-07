# The soak pre-flight's readiness check (#1323): reads `kubectl get pods
# --no-headers` (NAME READY STATUS ...) and prints one `NOT READY: <line>` per
# pod that is not Running with every container ready. Exits 1 if it printed
# any, 0 if every pod is ready, 2 if it was given no pods at all (an empty
# listing is a failed call or a wrong namespace, never "all ready").
#
# The workstation kickoff this replaces tested `$2 !~ /^([0-9]+)\/\1$/`. awk
# regular expressions have no back-references: `\1` there is the byte 0x01, so
# the pattern never matched and EVERY pod printed as NOT READY, which made the
# line noise and hid a real one. Split the column and compare the numbers.
#
# A Completed pod (a finished Job's) is skipped: it is not meant to be ready.
{
	seen++
	if ($3 == "Completed") next
	n = split($2, r, "/")
	if ($3 != "Running" || n != 2 || r[1] !~ /^[0-9]+$/ || r[2] !~ /^[0-9]+$/ || r[1] + 0 != r[2] + 0 || r[2] + 0 == 0) {
		print "NOT READY: " $0
		bad++
	}
}
END {
	if (seen == 0) exit 2
	exit (bad > 0)
}
