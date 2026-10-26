# Example: train on the iris dataset remotely

A complete remote run in three commands: send the data and script, train on the server, fetch the results. `train.py` needs only Python 3 and numpy on the server.

```bash
sshtab run  myserver/iris 'mkdir -p ~/iris && cd ~/iris'
sshtab push myserver/iris iris.csv train.py
sshtab run  myserver/iris 'python3 train.py'
sshtab pull myserver/iris results
```

```text
[exit 0 · myserver/iris · /home/me/iris]
loaded 150 samples, 3 classes
epoch 50 loss=0.2376
...
test accuracy: 0.967 (29/30)
```

`results/metrics.json` and `results/model.json` end up next to this README.

`iris.csv` is the Iris dataset by R. A. Fisher, from the [UCI Machine Learning Repository](https://archive.ics.uci.edu/dataset/53/iris), licensed under [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/).
