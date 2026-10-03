/* SPDX-License-Identifier: Apache-2.0 */
/* Keep the control daemon tied to its runtime supervisor, not its cgroup. */
#include <errno.h>
#include <limits.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/prctl.h>
#include <unistd.h>

int main(int argc, char **argv)
{
	char *end;
	long parent;

	if (argc < 3) {
		fprintf(stderr, "Usage: daemon-parent PARENT_PID COMMAND [ARG...]\n");
		return 2;
	}

	errno = 0;
	parent = strtol(argv[1], &end, 10);
	if (errno || *end || parent <= 1 || parent > INT_MAX) {
		fprintf(stderr, "Invalid supervisor PID\n");
		return 2;
	}

	if (prctl(PR_SET_PDEATHSIG, SIGTERM) < 0) {
		perror("prctl(PR_SET_PDEATHSIG)");
		return 1;
	}

	/* Close the window where the supervisor died before prctl completed. */
	if (getppid() != (pid_t)parent) {
		fprintf(stderr, "Runtime supervisor already exited\n");
		return 1;
	}

	execvp(argv[2], &argv[2]);
	perror("execvp");
	return 1;
}
