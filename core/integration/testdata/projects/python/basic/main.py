from pathlib import Path

import dagger
from dagger.server import command, commands


@command
def test_file(client: dagger.Client, file_prefix: str) -> dagger.File:
    name = f"{file_prefix}foo.txt"
    return client.directory().with_new_file(name, "foo\n").file(name)


@command
def test_dir(client: dagger.Client, dir_prefix: str) -> dagger.Directory:
    return (
        client.directory()
        .with_new_directory(f"{dir_prefix}subdir")
        .with_new_file(f"{dir_prefix}subdir/subbar1.txt", "subbar1\n")
        .with_new_file(f"{dir_prefix}subdir/subbar2.txt", "subbar2\n")
        .with_new_file(f"{dir_prefix}bar1.txt", "bar1\n")
        .with_new_file(f"{dir_prefix}bar2.txt", "bar2\n")
    )


@command
def test_imported_project_dir() -> str:
    return "\n".join(str(p) for p in Path().glob("**/*"))


@command
def test_export_local_dir(client: dagger.Client) -> dagger.Directory:
    return client.host().directory("./core/integration/testdata/projects/python/basic")


@commands
class Level3:
    @command
    def foo(self) -> str:
        return "hello from foo"

    @command
    def bar(self) -> str:
        return "hello from bar"


@commands
class Level2:
    @command
    def level3(self) -> Level3:
        return Level3()


@commands
class Level1:
    @command
    def level2(self) -> Level2:
        return Level2()


@command
def level1() -> Level1:
    return Level1()
