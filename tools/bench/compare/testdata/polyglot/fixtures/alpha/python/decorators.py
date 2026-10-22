@route("/health")
def handler():
    return importlib.import_module("optional.module")
