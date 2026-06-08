from logserve import actor, create_actor, get_actor_status, replay_actor


@actor(snapshot_every=20)
class Counter:
    def __init__(self):
        self.value = 0

    def inc(self):
        self.value += 1
        return self.value

    def get(self):
        return self.value


if __name__ == "__main__":
    counter = create_actor(Counter, snapshot_every=20)
    for expected in range(1, 101):
        actual = counter.inc()
        assert actual == expected, f"inc() returned {actual}, want {expected}"

    value = counter.get()
    assert value == 100

    status = get_actor_status(counter.actor_id)
    assert status["command_count"] >= 101
    assert status["snapshot_ref"]
    assert status["snapshot_command_count"] >= 100

    replay = replay_actor(counter.actor_id)
    assert replay["consistent_with_metadata"]
    assert replay["snapshot_replay_commands"] < replay["full_replay_commands"]

    print(value)
