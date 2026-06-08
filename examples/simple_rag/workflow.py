from logserve import submit, task, workflow


@task
def embed(query: str) -> str:
    return "vec:" + query


@task
def search(vec: str) -> list[str]:
    return ["doc:" + vec]


@task
def generate_mock(query: str, docs: list[str]) -> str:
    return "answer:" + query + ":" + docs[0]


@workflow
def simple_rag(query: str):
    vec = embed(query)
    docs = search(vec)
    ans = generate_mock(query, docs)
    return ans


if __name__ == "__main__":
    result = submit(simple_rag, "hello")
    assert result == "answer:hello:doc:vec:hello"
    print(result)
