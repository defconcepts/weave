#! /bin/bash

. ./config.sh

NAME=seetwo.weave.local

check_attached() {
    assert_raises "exec_on $HOST1 c2 $CHECK_ETHWE_UP"
    assert_dns_record $HOST1 c1 $NAME $C2
}

wait_for_proxy() {
    for i in $(seq 1 120); do
        echo "Waiting for proxy to start"
        if proxy docker_on $1 info > /dev/null 2>&1 ; then
            return
        fi
        sleep 1
    done
    echo "Timed out waiting for proxy to start" >&2
    exit 1
}

start_suite "Proxy restart reattaches networking to containers"

WEAVE_DOCKER_ARGS=--restart=always WEAVEPROXY_DOCKER_ARGS=--restart=always weave_on $HOST1 launch
proxy_start_container          $HOST1 -di --name=c2 --restart=always -h $NAME
proxy_start_container_with_dns $HOST1 -di --name=c1 --restart=always
C2=$(container_ip $HOST1 c2)

proxy docker_on $HOST1 restart -t=1 c2
check_attached

# Restart weave router
docker_on $HOST1 restart weave
sleep 1
check_attached

# Kill outside of Docker so Docker will restart it
run_on $HOST1 sudo kill -KILL $(docker_on $HOST1 inspect --format='{{.State.Pid}}' c2)
sleep 1
check_attached

# Disabled because Docker 1.8 often fails to restart weave or weaveproxy
# Restart docker itself, using different commands for systemd- and upstart-managed.
#run_on $HOST1 sh -c "command -v systemctl >/dev/null && sudo systemctl restart docker || sudo service docker restart"
#wait_for_proxy $HOST1
# Re-fetch the IP since it is not retained on docker restart
#C2=$(container_ip $HOST1 c2)
#check_attached

# Restarting proxy shouldn't kill unattachable containers
proxy_start_container $HOST1 -di --name=c3 --restart=always # Use ipam, so it won't be attachable w/o weave
weave_on $HOST1 stop
weave_on $HOST1 launch-proxy
assert_raises "exec_on $HOST1 c3 $CHECK_ETHWE_UP"

end_suite
