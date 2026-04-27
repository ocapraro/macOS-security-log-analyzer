#include <stdio.h>
#include <stdlib.h>
#include <EndpointSecurity/EndpointSecurity.h>
#include <bsm/libbsm.h>
#include <dispatch/dispatch.h>
#include <time.h>

int main(void) {
    es_client_t *client = NULL;

    es_new_client_result_t result = es_new_client(&client, ^(es_client_t *c, const es_message_t *msg) {
        long ts = (long)msg->time.tv_sec;

        switch (msg->event_type) {
            case ES_EVENT_TYPE_NOTIFY_EXEC: {
                const char *proc   = msg->process->executable->path.data;
                const char *target = msg->event.exec.target->executable->path.data;
                printf(
                    "{\"ts\":%ld,\"event\":\"exec\",\"pid\":%d,\"ppid\":%d,"
                    "\"process_path\":\"%s\",\"target_path\":\"%s\"}\n",
                    ts,
                    audit_token_to_pid(msg->process->audit_token),
                    msg->process->ppid,
                    proc   ? proc   : "",
                    target ? target : ""
                );
                fflush(stdout);
                break;
            }
            case ES_EVENT_TYPE_NOTIFY_FORK: {
                printf(
                    "{\"ts\":%ld,\"event\":\"fork\",\"pid\":%d,\"child_pid\":%d}\n",
                    ts,
                    audit_token_to_pid(msg->process->audit_token),
                    audit_token_to_pid(msg->event.fork.child->audit_token)
                );
                fflush(stdout);
                break;
            }
            case ES_EVENT_TYPE_NOTIFY_WRITE: {
                const char *path = msg->event.write.target->path.data;
                printf(
                    "{\"ts\":%ld,\"event\":\"write\",\"pid\":%d,\"target_path\":\"%s\"}\n",
                    ts,
                    audit_token_to_pid(msg->process->audit_token),
                    path ? path : ""
                );
                fflush(stdout);
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

    dispatch_main();
    return 0;
}