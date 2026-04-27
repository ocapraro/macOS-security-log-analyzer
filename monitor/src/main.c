#include <stdio.h>
#include <stdlib.h>
#include <EndpointSecurity/EndpointSecurity.h>
#include <bsm/libbsm.h>
#include <dispatch/dispatch.h>
#include <pthread.h>
#include <string.h>
#include <time.h>

#define MAX_EVENT_KEY 1024
#define MAX_EVENT_DETAILS 2048
#define EVENT_CACHE_SIZE 1024

typedef struct {
    int in_use;
    pid_t sample_pid;
    pid_t last_pid;
    char event[16];
    char key[MAX_EVENT_KEY];
    char details[MAX_EVENT_DETAILS];
    long first_ts;
    long last_ts;
    unsigned long count;
    unsigned long pid_count;
} event_cache_entry_t;

static event_cache_entry_t event_cache[EVENT_CACHE_SIZE];
static pthread_mutex_t event_cache_lock = PTHREAD_MUTEX_INITIALIZER;
static long summary_window_seconds = 10;

static void copy_token(char *dst, size_t dst_size, es_string_token_t token) {
    if (dst_size == 0) {
        return;
    }

    size_t len = token.length;
    if (len >= dst_size) {
        len = dst_size - 1;
    }

    if (token.data && len > 0) {
        memcpy(dst, token.data, len);
    }

    dst[len] = '\0';
}

static void json_escape(const char *src, char *dst, size_t dst_size) {
    if (dst_size == 0) {
        return;
    }

    size_t out = 0;
    for (size_t i = 0; src[i] && out + 1 < dst_size; i++) {
        unsigned char ch = (unsigned char)src[i];
        if ((ch == '"' || ch == '\\') && out + 2 < dst_size) {
            dst[out++] = '\\';
            dst[out++] = (char)ch;
        } else if (ch == '\n' && out + 2 < dst_size) {
            dst[out++] = '\\';
            dst[out++] = 'n';
        } else if (ch == '\r' && out + 2 < dst_size) {
            dst[out++] = '\\';
            dst[out++] = 'r';
        } else if (ch == '\t' && out + 2 < dst_size) {
            dst[out++] = '\\';
            dst[out++] = 't';
        } else if (ch >= 0x20) {
            dst[out++] = (char)ch;
        }
    }

    dst[out] = '\0';
}

static void emit_event(const event_cache_entry_t *entry) {
    printf(
        "{\"ts\":%ld,\"first_ts\":%ld,\"event\":\"%s\",\"count\":%lu,"
        "\"sample_pid\":%d,\"last_pid\":%d,\"pid_count\":%lu,%s}\n",
        entry->last_ts,
        entry->first_ts,
        entry->event,
        entry->count,
        entry->sample_pid,
        entry->last_pid,
        entry->pid_count,
        entry->details
    );
    fflush(stdout);
}

static void flush_summaries(long now, int force) {
    for (size_t i = 0; i < EVENT_CACHE_SIZE; i++) {
        event_cache_entry_t *entry = &event_cache[i];
        if (!entry->in_use || entry->count == 0) {
            continue;
        }

        if (force || now - entry->first_ts >= summary_window_seconds) {
            emit_event(entry);
            memset(entry, 0, sizeof(*entry));
        }
    }
}

static void record_event(long ts, const char *event, pid_t pid, const char *key, const char *details) {
    if (summary_window_seconds <= 0) {
        event_cache_entry_t entry = {0};
        entry.sample_pid = pid;
        entry.last_pid = pid;
        entry.pid_count = 1;
        entry.first_ts = ts;
        entry.last_ts = ts;
        entry.count = 1;
        snprintf(entry.event, sizeof(entry.event), "%s", event);
        snprintf(entry.details, sizeof(entry.details), "%s", details);
        emit_event(&entry);
        return;
    }

    event_cache_entry_t *slot = NULL;
    event_cache_entry_t *oldest = &event_cache[0];
    for (size_t i = 0; i < EVENT_CACHE_SIZE; i++) {
        event_cache_entry_t *entry = &event_cache[i];
        if (
            entry->in_use &&
            strcmp(entry->event, event) == 0 &&
            strcmp(entry->key, key) == 0
        ) {
            slot = entry;
            break;
        }

        if (!entry->in_use && slot == NULL) {
            slot = entry;
        }

        if (entry->in_use && entry->first_ts < oldest->first_ts) {
            oldest = entry;
        }
    }

    if (slot && slot->in_use) {
        slot->count++;
        if (slot->last_pid != pid) {
            slot->pid_count++;
        }
        slot->last_pid = pid;
        slot->last_ts = ts;
        snprintf(slot->details, sizeof(slot->details), "%s", details);
        return;
    }

    if (slot == NULL) {
        slot = oldest;
        if (slot->in_use && slot->count > 0) {
            emit_event(slot);
        }
    }

    memset(slot, 0, sizeof(*slot));
    slot->in_use = 1;
    slot->sample_pid = pid;
    slot->last_pid = pid;
    slot->pid_count = 1;
    slot->first_ts = ts;
    slot->last_ts = ts;
    slot->count = 1;
    snprintf(slot->event, sizeof(slot->event), "%s", event);
    snprintf(slot->key, sizeof(slot->key), "%s", key);
    snprintf(slot->details, sizeof(slot->details), "%s", details);
}

int main(void) {
    es_client_t *client = NULL;
    const char *summary_env = getenv("ES_SUMMARY_WINDOW");
    if (summary_env) {
        summary_window_seconds = strtol(summary_env, NULL, 10);
    }

    es_new_client_result_t result = es_new_client(&client, ^(es_client_t *c, const es_message_t *msg) {
        long ts = (long)msg->time.tv_sec;
        pid_t pid = audit_token_to_pid(msg->process->audit_token);

        switch (msg->event_type) {
            case ES_EVENT_TYPE_NOTIFY_EXEC: {
                char proc[MAX_EVENT_KEY];
                char target[MAX_EVENT_KEY];
                char proc_json[MAX_EVENT_KEY * 2];
                char target_json[MAX_EVENT_KEY * 2];
                char key[MAX_EVENT_KEY];
                char details[MAX_EVENT_DETAILS];

                copy_token(proc, sizeof(proc), msg->process->executable->path);
                copy_token(target, sizeof(target), msg->event.exec.target->executable->path);
                json_escape(proc, proc_json, sizeof(proc_json));
                json_escape(target, target_json, sizeof(target_json));
                snprintf(key, sizeof(key), "%s|%s", proc, target);

                snprintf(
                    details,
                    sizeof(details),
                    "\"last_ppid\":%d,\"process_path\":\"%s\",\"target_path\":\"%s\"",
                    msg->process->ppid,
                    proc_json,
                    target_json
                );
                pthread_mutex_lock(&event_cache_lock);
                record_event(ts, "exec", pid, key, details);
                pthread_mutex_unlock(&event_cache_lock);
                break;
            }
            case ES_EVENT_TYPE_NOTIFY_FORK: {
                pid_t child_pid = audit_token_to_pid(msg->event.fork.child->audit_token);
                char proc[MAX_EVENT_KEY];
                char proc_json[MAX_EVENT_KEY * 2];
                char key[MAX_EVENT_KEY];
                char details[MAX_EVENT_DETAILS];

                copy_token(proc, sizeof(proc), msg->process->executable->path);
                json_escape(proc, proc_json, sizeof(proc_json));
                snprintf(key, sizeof(key), "%s", proc);
                snprintf(
                    details,
                    sizeof(details),
                    "\"process_path\":\"%s\",\"sample_child_pid\":%d",
                    proc_json,
                    child_pid
                );
                pthread_mutex_lock(&event_cache_lock);
                record_event(ts, "fork", pid, key, details);
                pthread_mutex_unlock(&event_cache_lock);
                break;
            }
            case ES_EVENT_TYPE_NOTIFY_WRITE: {
                char proc[MAX_EVENT_KEY];
                char path[MAX_EVENT_KEY];
                char proc_json[MAX_EVENT_KEY * 2];
                char path_json[MAX_EVENT_KEY * 2];
                char key[MAX_EVENT_KEY];
                char details[MAX_EVENT_DETAILS];

                copy_token(proc, sizeof(proc), msg->process->executable->path);
                copy_token(path, sizeof(path), msg->event.write.target->path);
                json_escape(proc, proc_json, sizeof(proc_json));
                json_escape(path, path_json, sizeof(path_json));
                snprintf(key, sizeof(key), "%s|%s", proc, path);

                snprintf(
                    details,
                    sizeof(details),
                    "\"process_path\":\"%s\",\"target_path\":\"%s\"",
                    proc_json,
                    path_json
                );
                pthread_mutex_lock(&event_cache_lock);
                record_event(ts, "write", pid, key, details);
                pthread_mutex_unlock(&event_cache_lock);
                break;
            }
            default:
                break;
        }
    });

    if (result != ES_NEW_CLIENT_RESULT_SUCCESS) {
        fprintf(stderr, "{\"error\":\"es_new_client failed\",\"code\":%d}\n", result);
        return 1;
    }

    es_event_type_t events[] = {
        ES_EVENT_TYPE_NOTIFY_EXEC,
        ES_EVENT_TYPE_NOTIFY_FORK,
        ES_EVENT_TYPE_NOTIFY_WRITE,
    };

    if (es_subscribe(client, events, sizeof(events) / sizeof(events[0])) != ES_RETURN_SUCCESS) {
        fprintf(stderr, "{\"error\":\"es_subscribe failed\"}\n");
        es_delete_client(client);
        return 1;
    }

    fprintf(stderr, "{\"status\":\"ok\"}\n");

    if (summary_window_seconds > 0) {
        dispatch_source_t timer = dispatch_source_create(
            DISPATCH_SOURCE_TYPE_TIMER,
            0,
            0,
            dispatch_get_global_queue(QOS_CLASS_UTILITY, 0)
        );
        dispatch_source_set_timer(
            timer,
            dispatch_time(DISPATCH_TIME_NOW, summary_window_seconds * NSEC_PER_SEC),
            summary_window_seconds * NSEC_PER_SEC,
            NSEC_PER_SEC
        );
        dispatch_source_set_event_handler(timer, ^{
            pthread_mutex_lock(&event_cache_lock);
            flush_summaries((long)time(NULL), 1);
            pthread_mutex_unlock(&event_cache_lock);
        });
        dispatch_resume(timer);
    }

    dispatch_main();
    return 0;
}
