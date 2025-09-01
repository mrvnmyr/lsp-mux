lsp-mux
=======

A tiny Language Server Protocol (LSP) multiplexer for Vim/Neovim that reuses
a single language server process **per project** and **per language** across
multiple editor instances.

Why?
----
Many editor setups (e.g. YouCompleteMe in Vim) spawn a fresh language server
for each editor instance:

.. code-block:: vim

   let g:ycm_language_server =
   \ [
   \   {
   \     'name': 'kotlin',
   \     'cmdline': [ '/usr/bin/kotlin-lsp', '--stdio' ],
   \     'filetypes': [ 'kotlin' ],
   \   },
   \ ]

This wastes resources when you have several Vim instances open on the same
project. **lsp-mux** lets all those editor instances share one server per
project, keeping it alive for at least 10 minutes after the last editor
detaches.

How it works
------------
* Place a ``.lspmux`` file at your project root (e.g.
  ``~/workspace/my-kot-project/.lspmux``).
* Replace your server command with ``lsp-mux``. The first Vim that connects
  for a given (project, language) spawns the real language server and a Unix
  domain socket. Later Vims connect to that mux instead of spawning another
  server.
* The mux proxies JSON-RPC/LSP between all clients and the single server and
  takes care of request/response ID mapping. It forwards only one
  ``initialize`` handshake to the server and locally replies to additional
  clients with the cached result. It keeps the server alive for a minimum of
  10 minutes after the last client disconnects.

Status / scope
--------------
This is a pragmatic, minimal proxy designed for stdio LSP servers. It
deliberately:
  * Supports Linux/macOS via Unix sockets.
  * Shares one server per **project + language** (multiple projects at once
    work fine).
  * Broadcasts server notifications (e.g. diagnostics) to all clients.
  * Routes server-initiated requests to the *primary* client.

Quick start
-----------
Build:

.. code-block:: bash

   go build -o lsp-mux ./cmd/lsp-mux

Project setup:

.. code-block:: bash

   touch ~/workspace/my-kot-project/.lspmux

Neovim/YouCompleteMe example (Kotlin):

.. code-block:: vim

   let g:ycm_language_server =
   \ [
   \   {
   \     'name': 'kotlin',
   \     'cmdline': [ 'lsp-mux', '--lang', 'kotlin', '--server', '/usr/bin/kotlin-lsp', '--', '--stdio' ],
   \     'filetypes': [ 'kotlin' ],
   \   },
   \ ]

Nim example:

.. code-block:: vim

   let g:ycm_language_server +=
   \ [
   \   {
   \     'name': 'nim',
   \     'cmdline': [ 'lsp-mux', '--lang', 'nim', '--server', 'nimlsp' ],
   \     'filetypes': [ 'nim' ],
   \   },
   \ ]

Behavior details
----------------
* **Project detection:** lsp-mux searches upward from the path provided in
  the client's ``initialize`` params (``rootUri``, ``rootPath`` or the first
  workspace folder) for a ``.lspmux`` file and uses that directory as the
  project tag. If none is found, it falls back to the nearest directory.
* **Socket placement:** sockets live under ``$XDG_RUNTIME_DIR/lsp-mux`` or
  ``~/.cache/lsp-mux``.
* **Linger:** when the last client disconnects, the server stays alive for
  at least 10 minutes (configurable via ``--linger``) before a graceful
  shutdown (``shutdown`` request + ``exit`` notification).
* **Multiple languages:** pass ``--lang <k>`` and the real server via
  ``--server ... [-- extra args]`` for each filetype you configure. You can
  run multiple different language servers across multiple projects at once.

Caveats
-------
* Server-initiated requests (e.g. ``workspace/configuration``) are routed to
  the *first* client that connected (the "primary"). If that client closes,
  another becomes primary.
* This proxy replies locally to additional clients' ``initialize`` requests
  using the cached result from the first handshake. Most servers are fine
  with this because they only see one logical client.

Flags
-----
.. code-block:: text

   lsp-mux --lang <k> --server <bin> [-- <args...>]
     --tag-file <name>     Marker file (default: .lspmux)
     --socket-dir <dir>    Socket directory (default: $XDG_RUNTIME_DIR/lsp-mux or ~/.cache/lsp-mux)
     --linger <dur>        Idle linger time (default: 10m)
     -v                    Verbose logging

Example:

.. code-block:: bash

   lsp-mux --lang kotlin --server /usr/bin/kotlin-lsp -- --stdio
