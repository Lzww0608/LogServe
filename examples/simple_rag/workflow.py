# Small workflow example that composes local Python task functions through the
# LogServe SDK without requiring a model registry or LLM worker.
from logserve import submit, task, workflow


# embed simulates vectorization while staying deterministic for workflow replay.
@task
def embed(query: str) -> str:
    return "vec:" + query


# search returns a stable single-document result so downstream assertions can
# prove task ordering and argument passing.
@task
def search(vec: str) -> list[str]:
    return ["doc:" + vec]


# generate_mock stands in for answer synthesis without touching LLM routing.
@task
def generate_mock(query: str, docs: list[str]) -> str:
    return "answer:" + query + ":" + docs[0]


# simple_rag demonstrates a three-step workflow DAG encoded as ordinary Python
# calls; the SDK captures those calls as workflow steps.
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
