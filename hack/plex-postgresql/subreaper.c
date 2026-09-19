/* subreaper: run a command and outlive its re-exec.
 *
 * Plex re-execs itself through vfork. The process we started exits cleanly and
 * its replacement is reparented to pid 1, so a supervisor watching only what
 * it launched sees a healthy exit and restarts the pod, on a loop.
 *
 * PR_SET_CHILD_SUBREAPER makes orphaned descendants reparent here instead, and
 * we wait until there are none left rather than until the first child exits.
 * The exit status reported is the one the direct child gave, so a genuine
 * crash still reads as a crash.
 *
 * Built here rather than taken from upstream: only some releases of
 * plex-postgresql ship it, and we depend on it in all of them.
 */
#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <sys/prctl.h>
#include <sys/wait.h>
#include <unistd.h>

static volatile pid_t child_pid = 0;

/* Pass signals to the whole group, so a stop reaches the re-exec'd Plex and
 * not just the process we happen to have started. */
static void forward(int sig) { kill(0, sig); }

int main(int argc, char **argv) {
  if (argc < 2) {
    fprintf(stderr, "usage: subreaper command [args...]\n");
    return 1;
  }

  prctl(PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0);
  signal(SIGTERM, forward);
  signal(SIGINT, forward);
  signal(SIGHUP, forward);

  child_pid = fork();
  if (child_pid == 0) {
    execvp(argv[1], argv + 1);
    perror("subreaper: exec");
    _exit(127);
  }
  if (child_pid < 0) {
    perror("subreaper: fork");
    return 1;
  }

  int status = 0, exit_code = 0;
  pid_t pid;
  /* Wait for every descendant, not just the one we forked: after a re-exec the
   * replacement is ours to reap, and exiting before it would defeat the point. */
  while ((pid = wait(&status)) > 0 || (pid < 0 && errno == EINTR)) {
    if (pid <= 0) {
      continue;
    }
    if (pid == child_pid) {
      exit_code = WIFEXITED(status) ? WEXITSTATUS(status) : 128 + WTERMSIG(status);
      child_pid = 0; /* nothing left to forward signals to */
    }
  }
  return exit_code;
}
