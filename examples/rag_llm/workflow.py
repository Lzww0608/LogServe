# RAG + LLM example that registers a mock model, selects locality-aware
# scheduling, and submits a workflow containing both task steps and LLM replay data.
from logserve import (
    llm_generate,
    register_model,
    set_scheduling_policy,
    submit,
    task,
    workflow,
)


# embed produces a deterministic vector token for replayable workflow inputs.
@task
def embed(query: str) -> str:
    return "vec:" + query


# search returns one document so the prompt builder has a stable dependency.
@task
def search(vec: str) -> list[str]:
    return ["doc:" + vec]


# build_prompt isolates prompt construction from the LLM step so workflow traces
# show the boundary between ordinary tasks and model execution.
@task
def build_prompt(query: str, docs: list[str]) -> str:
    return "answer " + query + " using " + docs[0]


# rag_with_llm ends with llm_generate so replay can recover cache/model metadata
# in addition to ordinary task outputs.
@workflow
def rag_with_llm(query: str):
    vec = embed(query)
    docs = search(vec)
    prompt = build_prompt(query, docs)
    return llm_generate("model-A", prompt, version="v1", adapter="mock", step_id="generate_answer")


if __name__ == "__main__":
    # Register before submit so scheduler/model lookup sees the model capability.
    register_model("model-A", version="v1", size_bytes=100, path="mock://model-A", adapter="mock")
    set_scheduling_policy("LOCALITY_AWARE")
    result = submit(rag_with_llm, "hello")
    assert "mock:model-A" in result
    print(result)
