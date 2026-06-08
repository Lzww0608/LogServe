from logserve import (
    llm_generate,
    register_model,
    set_scheduling_policy,
    submit,
    task,
    workflow,
)


@task
def embed(query: str) -> str:
    return "vec:" + query


@task
def search(vec: str) -> list[str]:
    return ["doc:" + vec]


@task
def build_prompt(query: str, docs: list[str]) -> str:
    return "answer " + query + " using " + docs[0]


@workflow
def rag_with_llm(query: str):
    vec = embed(query)
    docs = search(vec)
    prompt = build_prompt(query, docs)
    return llm_generate("model-A", prompt, version="v1", adapter="mock", step_id="generate_answer")


if __name__ == "__main__":
    register_model("model-A", version="v1", size_bytes=100, path="mock://model-A", adapter="mock")
    set_scheduling_policy("LOCALITY_AWARE")
    result = submit(rag_with_llm, "hello")
    assert "mock:model-A" in result
    print(result)
