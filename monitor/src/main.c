#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <time.h>

void emit_exec(int pid, int ppid, const char *proc, const char *target) {
    printf(
        "{\"ts\":%ld,\"event\":\"exec\",\"pid\":%d,\"ppid\":%d,"
        "\"process_path\":\"%s\",\"target_path\":\"%s\"}\n",
        time(NULL), pid, ppid, proc, target
    );
    fflush(stdout);
}

void emit_fork(int parent, int child) {
    printf(
        "{\"ts\":%ld,\"event\":\"fork\",\"pid\":%d,\"child_pid\":%d}\n",
        time(NULL), parent, child
    );
    fflush(stdout);
}

void emit_write(int pid, const char *path) {
    printf(
        "{\"ts\":%ld,\"event\":\"write\",\"pid\":%d,\"target_path\":\"%s\"}\n",
        time(NULL), pid, path
    );
    fflush(stdout);
}

int main() {
    int base_pid = 1000;

    printf("Starting dummy event stream...\n");

    while (1) {
        int shell = base_pid++;
        int curl = base_pid++;
        int payload = base_pid++;

        // Simulate: bash -> curl -> download -> execute
        emit_exec(shell, 1, "/bin/bash", "/bin/bash");
        sleep(1);

        emit_fork(shell, curl);
        emit_exec(curl, shell, "/usr/bin/curl", "http://malicious.com/payload");
        sleep(1);

        emit_write(curl, "/tmp/payload");
        sleep(1);

        emit_exec(payload, curl, "/tmp/payload", "/tmp/payload");
        sleep(3);
    }

    return 0;
}