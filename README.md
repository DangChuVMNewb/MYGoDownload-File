# Go-Dowloader

[![Repo Size](https://img.shields.io/github/repo-size/DangChuVMNewb/MYGoDownload-File)](https://github.com/DangChuVMNewb/MYGoDownload-File)
[![Last Commit](https://img.shields.io/github/last-commit/DangChuVMNewb/MYGoDownload-File)](https://github.com/DangChuVMNewb/MYGoDownload-File/commits)
[![Contributors](https://img.shields.io/github/contributors/DangChuVMNewb/MYGoDownload-File)](https://github.com/DangChuVMNewb/MYGoDownload-File/graphs/contributors)

A simple command-line tool for downloading files with support for concurrent and resumable downloads.

## Features

- **Concurrent Downloads:** Utilizes multiple threads to download files faster.
- **Resumable Downloads:** Can resume interrupted downloads.
- **Progress Bar:** Displays a progress bar with download speed, percentage, and ETA.
- **Easy to Use:** Simple and intuitive command-line interface.

## Installation

To build the project, you need to have Go installed.

```bash
go build
```

## Usage

```bash
./dow [flags] <filepath> <URL>
```

Or

```bash
./dow [flags] <URL>
```

### Flags

- `-c`: Resume download if possible.
- `-th`: Number of threads to use (default 2).

### Examples

- **Download a file:**

```bash
./dow https://example.com/file.zip
```

- **Download a file with a specific name:**

```bash
./dow my-file.zip https://example.com/file.zip
```

- **Download a file with 4 threads:**

```bash
./dow -th 4 https://example.com/large-file.zip
```

- **Resume a download:**

```bash
./dow -c https://example.com/large-file.zip
```
