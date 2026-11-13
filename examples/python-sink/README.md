# python-sink

An `acplug/1` event sink in Python 3, standard library only, over the HTTP/JSON mapping. It deduplicates events by id and can append them to a JSON Lines file.

```
SINK_TOKEN=change-me python3 plugin.py --port 8099 --token-env SINK_TOKEN --out events.jsonl
SINK_TOKEN=change-me bin/conform -endpoint http://127.0.0.1:8099 -protocol http -insecure -credential env:SINK_TOKEN -report report.json
```

`cmd/conform/main_test.go` runs exactly this in `make test`. In a deployment, put it behind TLS (`https://` endpoint with `tls.ca`) and select it with `sink: {use: plugin:python-sink}`.
