# hello-python

A minimal Flask hello-world for gregale.

## Deploy

```
gregale deploy --template hello-python
```

imaged will detect `requirements.txt` and use the `python312` runner.

## Try it

```
gregale open             # browser, or:
gregale curl <slug>      # print first 200 bytes (if available)
```

## Edit and re-deploy

Edit `handler.py`, then re-run `gregale deploy --template hello-python --name <slug>`.