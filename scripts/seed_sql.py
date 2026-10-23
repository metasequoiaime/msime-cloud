"""社区种子脚本共用的 SQL 文本生成工具。"""


def literal(value):
    """沿用种子脚本的字符串字面量规则，仅将单引号加倍。"""
    return "'" + value.replace("'", "''") + "'"


def do_block(body):
    """选择正文中不存在的美元引用分隔符，避免数据提前结束匿名代码块。"""
    delimiter = "$$"
    index = 0
    while delimiter in body:
        index += 1
        delimiter = f"$seed_{index}$"
    return f"DO {delimiter} {body} {delimiter};"
