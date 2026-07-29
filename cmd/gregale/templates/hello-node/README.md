# hello-node

A minimal Express.js hello-world for gregale.

## Deploy

From this directory:

```
gregale deploy --template hello-node
```

This materializes the template, tars it, and ships it to apid. imaged
will detect `package.json` and use the `node22` runner.

## Try it

```
gregale open             # browser, or:
gregale curl <slug>      # print first 200 bytes (if available)
```

## Edit and re-deploy

```
# edit handler.js, then:
gregale deploy --template hello-node --name <slug>
```

## Add secrets

```
gregale env push --app <slug> -f .env
```

The handler's `/` endpoint echoes the secret key names (not values) so
you can confirm the push landed.