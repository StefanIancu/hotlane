from fastapi import FastAPI
from fastapi.responses import PlainTextResponse

VERSION = "v1"

app = FastAPI()


@app.get("/health", response_class=PlainTextResponse)
def health() -> str:
    return "ok\n"


@app.get("/", response_class=PlainTextResponse)
def root() -> str:
    return f"hello from bench-fastapi {VERSION}\n"
