"""Minimal Matplotlib rendering evaluator for the Pyodide route."""

from io import BytesIO

import matplotlib

matplotlib.use("agg")
import matplotlib.pyplot as plt


def evaluation_function(response, answer, params):
    values = params.get("values", [0, 1, 4, 9])
    figure, axis = plt.subplots()
    axis.plot(range(len(values)), values)
    rendered = BytesIO()
    figure.savefig(rendered, format="png")
    plt.close(figure)
    return {
        "is_correct": rendered.tell() > 1_000,
        "png_bytes": rendered.tell(),
    }
