# npower-validacao

Power de validação. Serve só para testar como o Kiro instala e executa um Power (pasta de dados, placeholders, confirmação de tools, reinício do servidor, atualização do tool list). Não contém código de produto.

Tools: `ping`, `info`, `hello_status`, `hello_install`, `write_marker`, `read_marker`, `log_tail`, `restart`.

Código-fonte do servidor em `src/` (Go, só biblioteca padrão). `./build.sh` recompila `bin/`.
