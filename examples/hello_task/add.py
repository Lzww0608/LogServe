# Minimal task submission example: define one SDK task and execute it through
# the configured LogServe control plane.
from logserve import submit, task


# add is intentionally pure so the smoke script can verify task execution
# without relying on external state or side effects.
@task
def add(a: int, b: int) -> int:
    return a + b


if __name__ == "__main__":
    result = submit(add, 1, 2)
    assert result == 3
    print(result)
