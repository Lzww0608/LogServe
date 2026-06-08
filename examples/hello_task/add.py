from logserve import submit, task


@task
def add(a: int, b: int) -> int:
    return a + b


if __name__ == "__main__":
    result = submit(add, 1, 2)
    assert result == 3
    print(result)
