"""Softmax regression on the iris dataset, with numpy only.

Reads iris.csv, trains on 80% of it, tests on the rest, and writes
results/metrics.json and results/model.json.
"""
import json
import os

import numpy as np

rng = np.random.default_rng(0)

rows = [line.strip().split(",") for line in open("iris.csv") if line.strip()]
features = np.array([[float(v) for v in row[:4]] for row in rows])
classes = sorted({row[4] for row in rows})
labels = np.array([classes.index(row[4]) for row in rows])
print(f"loaded {len(rows)} samples, {len(classes)} classes")

order = rng.permutation(len(rows))
train, test = order[:120], order[120:]
mean, std = features[train].mean(axis=0), features[train].std(axis=0)
x = np.hstack([(features - mean) / std, np.ones((len(rows), 1))])  # plus a bias column

weights = np.zeros((x.shape[1], len(classes)))
onehot = np.eye(len(classes))[labels[train]]
for epoch in range(1, 301):
    logits = x[train] @ weights
    probs = np.exp(logits - logits.max(axis=1, keepdims=True))
    probs /= probs.sum(axis=1, keepdims=True)
    loss = -np.log(probs[np.arange(len(train)), labels[train]]).mean()
    weights -= 0.5 * x[train].T @ (probs - onehot) / len(train)
    if epoch % 50 == 0:
        print(f"epoch {epoch} loss={loss:.4f}")

predicted = (x[test] @ weights).argmax(axis=1)
accuracy = float((predicted == labels[test]).mean())
confusion = np.zeros((len(classes), len(classes)), dtype=int)
for actual, guess in zip(labels[test], predicted):
    confusion[actual, guess] += 1
print(f"test accuracy: {accuracy:.3f} ({(predicted == labels[test]).sum()}/{len(test)})")
print("confusion matrix (rows: actual, columns: predicted):")
for name, row in zip(classes, confusion):
    print(f"  {name:<16} {row}")

os.makedirs("results", exist_ok=True)
with open("results/metrics.json", "w") as f:
    json.dump({"accuracy": accuracy, "test_samples": len(test), "classes": classes,
               "confusion": confusion.tolist()}, f, indent=2)
with open("results/model.json", "w") as f:
    json.dump({"weights": weights.tolist(), "mean": mean.tolist(), "std": std.tolist()}, f)
print("saved results/metrics.json and results/model.json")
