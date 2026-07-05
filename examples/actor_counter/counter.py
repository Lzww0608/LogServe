# Actor snapshot/replay example used by smoke scripts to verify command logging,
# snapshot materialization, and metadata consistency for actor state.
from logserve import actor, create_actor, get_actor_status, replay_actor


# Counter is a tiny mutable actor whose state is easy to validate after replay;
# snapshot_every is low enough that 100 increments must produce a snapshot.
@actor(snapshot_every=20)
class Counter:
    # __init__ defines the durable actor state captured by snapshots and replay.
    def __init__(self):
        self.value = 0

    # inc mutates actor state and returns the new value so command ordering is observable.
    def inc(self):
        self.value += 1
        return self.value

    # get is a read command used to confirm state after the increment sequence.
    def get(self):
        return self.value


if __name__ == "__main__":
    counter = create_actor(Counter, snapshot_every=20)
    for expected in range(1, 101):
        actual = counter.inc()
        assert actual == expected, f"inc() returned {actual}, want {expected}"

    value = counter.get()
    assert value == 100

    # Status checks prove the snapshot boundary was crossed before replay is measured.
    status = get_actor_status(counter.actor_id)
    assert status["command_count"] >= 101
    assert status["snapshot_ref"]
    assert status["snapshot_command_count"] >= 100

    # Snapshot replay should need fewer commands than full replay once metadata
    # points at the latest snapshot.
    replay = replay_actor(counter.actor_id)
    assert replay["consistent_with_metadata"]
    assert replay["snapshot_replay_commands"] < replay["full_replay_commands"]

    print(value)
