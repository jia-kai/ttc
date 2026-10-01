import csv
from pathlib import Path
from statistics import mean

with Path("observations.csv").open() as data:
    values = [float(row["value"]) for row in csv.DictReader(data)]
print(f"Mean: {mean(values):.1f}")
