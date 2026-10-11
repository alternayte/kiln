# kiln-sdk

The Python client for [Kiln](https://github.com/alternayte/kiln): isolated
Linux microVMs that restore from a snapshot, fork, sleep, and serve a port on
a public hostname.

```sh
pip install kiln-sdk
```

```python
from kiln import Kiln

kiln = Kiln("https://api.example.com", api_key="kiln_...")
kiln.build_template(name="py312", image="docker.io/library/python:3.12-slim",
                    vcpus=1, memory_mb=512, disk_mb=2048, egress_allow=[])
sandbox = kiln.create_sandbox_object(template="py312", lifecycle="ephemeral", idle_seconds=600)
print(sandbox.exec(["python3", "-c", "print(2+2)"]).stdout)
sandbox.write_file("/work/hello.txt", "hi\n")
print(sandbox.read_file("/work/hello.txt"))
sandbox.destroy()
```

`exec` streams when you pass `on_output`. `kiln.events(after=0)` iterates the
event stream. Every method of the generated client is on `Kiln` too; the
[API reference](https://github.com/alternayte/kiln/blob/main/sdk/llms-full.txt)
lists them.

MIT licence. The Kiln host and gateway are AGPL-3.0; this client is not.
