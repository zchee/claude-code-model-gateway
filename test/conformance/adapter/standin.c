// Copyright 2026 The claude-code-model-gateway Authors.
// SPDX-License-Identifier: Apache-2.0
#include <stdio.h>
#include <string.h>

extern char **environ;

// A private pipe to the test parent, never TAP or the persistent launch log.
int main(int argc, char **argv) {
  if (printf("%d\n", argc - 1) < 0)
    return 1;
  for (int i = 1; i < argc; i++) {
    if (fwrite(argv[i], 1, strlen(argv[i]) + 1, stdout) != strlen(argv[i]) + 1)
      return 1;
  }
  for (char **e = environ; *e; e++) {
    if (fwrite(*e, 1, strlen(*e) + 1, stdout) != strlen(*e) + 1)
      return 1;
  }
  return fflush(stdout) != 0;
}
